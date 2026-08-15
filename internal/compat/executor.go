// executor.go — CompatExecutor executes a playbook (dsl.Workflow) imported
// through the compatibility layer. It is part of package compat.
//
// The executor walks the workflow steps, simulates execution (MVP stage — no
// real target connection), and records an audit trace for every step via the
// TraceRecorder. Approval and gate requirements are recorded but not enforced
// (MVP). The supported module set is shell, command, file, copy and template,
// matching the minimal Ansible subset from T056.

package compat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
)

// --- sentinel errors -------------------------------------------------------

// Sentinel errors returned by the executor. ErrEmptyWorkflow is shared with
// risk.go (risk assessor); it is not redeclared here. Callers may use
// errors.Is to distinguish failure modes robust to wrapping.
var (
	// ErrExecFailed is returned when one or more steps failed; it is also
	// set on CompatExecResult.Error when Success is false.
	ErrExecFailed = errors.New("compat: execution failed")
	// ErrNoTargets is returned when the target list is empty.
	ErrNoTargets = errors.New("compat: no targets")
)

// supportedModules lists the modules supported by the MVP executor. All
// supported modules are simulated as successful in the MVP stage; modules
// outside this set are reported as failed (ErrExecFailed) so that callers
// can detect unsupported constructs rather than silently skipping them.
var supportedModules = map[string]bool{
	"shell":    true,
	"command":  true,
	"file":     true,
	"copy":     true,
	"template": true,
}

// --- CompatExecutor --------------------------------------------------------

// CompatExecutor executes a playbook (dsl.Workflow) imported through the
// compatibility layer. It generates a run id, creates a run row, then
// executes each step against each target (simulated in MVP), recording an
// audit trace per step. Approval and gate requirements are recorded but not
// enforced (MVP stage).
//
// The executor depends only on internal/dsl, internal/audit and internal/state
// (the latter is required to create the run row that backs the trace foreign
// key). It never imports internal/executor, internal/channel or internal/engine,
// satisfying the R8 independence constraint.
//
// The zero value is not usable; callers must use NewCompatExecutor.
type CompatExecutor struct {
	// recorder records audit traces for every executed step, gate check and
	// approval decision.
	recorder *audit.TraceRecorder
	// store is used to create the run row that satisfies the trace table's
	// run_id foreign-key constraint. It is the same store backing recorder.
	store state.Store
}

// CompatExecResult is the result of executing a playbook. It aggregates the
// per-step outcomes and an overall success flag.
type CompatExecResult struct {
	RunID        string           // run id (32-char hex)
	WorkflowName string           // workflow name (from wf.Meta.Name)
	Steps        []StepExecResult // one entry per target × step
	Success      bool             // true when every step succeeded
	Duration     time.Duration    // total execution wall-clock time
	Error        error            // ErrExecFailed when !Success, nil otherwise
}

// StepExecResult is the result of executing one step on one target.
type StepExecResult struct {
	Name     string        // step name
	Module   string        // module name (shell, command, file, ...)
	Action   string        // action name (exec, manage, copy, ...)
	Target   string        // target host
	Success  bool          // whether the step succeeded
	Duration time.Duration // step wall-clock time
	Error    error         // error (nil on success)
}

// NewCompatExecutor creates a CompatExecutor that records audit traces via
// recorder and persists the run row via store. Both must be non-nil; a nil
// store will cause Execute to panic on run creation, and a nil recorder will
// cause trace recording to panic. Callers typically obtain recorder from
// audit.NewTraceRecorder(store) using the same store.
func NewCompatExecutor(store state.Store, recorder *audit.TraceRecorder) *CompatExecutor {
	return &CompatExecutor{recorder: recorder, store: store}
}

// Execute runs the workflow against the given targets. It generates a run id,
// creates a run row, then executes each step against each target (simulated),
// recording an audit trace per step. Steps are executed in target-major order:
// for each target, all steps run in sequence. The overall result aggregates
// per-step outcomes; Success is true only when every step on every target
// succeeded.
//
// MVP stage: execution is simulated — no real connection is made to the
// targets. Supported modules (shell, command, file, copy, template) always
// succeed; unsupported modules fail with ErrExecFailed. Approval and gate
// requirements are recorded as audit traces but do not block execution.
func (e *CompatExecutor) Execute(ctx context.Context, wf *dsl.Workflow, targets []string) (*CompatExecResult, error) {
	if wf == nil || len(wf.Steps) == 0 {
		return nil, ErrEmptyWorkflow
	}
	if len(targets) == 0 {
		return nil, ErrNoTargets
	}

	runID, err := newRunID()
	if err != nil {
		return nil, fmt.Errorf("compat: generate run id: %w", err)
	}

	// Create the run row so that trace records satisfy the run_id foreign key.
	now := time.Now().UTC()
	if err := e.store.CreateRun(ctx, &state.Run{
		ID:           runID,
		WorkflowName: wf.Meta.Name,
		Status:       "running",
		CreatedAt:    now,
		UpdatedAt:    now,
		Creator:      "system",
	}); err != nil {
		return nil, fmt.Errorf("compat: create run: %w", err)
	}

	start := time.Now()
	result := &CompatExecResult{
		RunID:        runID,
		WorkflowName: wf.Meta.Name,
		Success:      true,
	}

	// Execute target-major: for each target, run all steps in sequence.
	for _, target := range targets {
		for _, step := range wf.Steps {
			sr := e.executeStep(ctx, runID, target, step)
			result.Steps = append(result.Steps, sr)
			if !sr.Success {
				result.Success = false
			}
		}
	}

	result.Duration = time.Since(start)
	// Ensure a strictly positive duration. On platforms with coarse clocks
	// (e.g. Windows ~1ms granularity) a simulated execution can complete
	// within the same tick, yielding a zero duration. A zero duration would
	// make it impossible for callers to distinguish "executed" from "not
	// executed", so we floor at one nanosecond.
	if result.Duration == 0 {
		result.Duration = time.Nanosecond
	}
	if !result.Success {
		result.Error = ErrExecFailed
	}
	return result, nil
}

// executeStep simulates execution of one step on one target and records the
// audit trace. MVP: supported modules succeed; unsupported modules fail. The
// trace is recorded best-effort — a recording failure does not affect the
// step outcome, because audit is a side-channel that must not alter execution
// semantics. Approval and gate requirements are likewise recorded best-effort
// and non-blocking.
func (e *CompatExecutor) executeStep(ctx context.Context, runID, target string, step dsl.Step) StepExecResult {
	sr := StepExecResult{
		Name:   step.Name,
		Module: step.Module,
		Action: step.Action,
		Target: target,
	}
	stepStart := time.Now()

	// MVP: simulate. Supported modules succeed; unsupported fail.
	if supportedModules[step.Module] {
		sr.Success = true
	} else {
		sr.Success = false
		sr.Error = fmt.Errorf("%w: unsupported module %q", ErrExecFailed, step.Module)
	}
	sr.Duration = time.Since(stepStart)

	// Record the step execution audit trace (best-effort).
	action := step.Module + "." + step.Action
	output := map[string]any{"success": sr.Success}
	if sr.Error != nil {
		output["error"] = sr.Error.Error()
	}
	_, _ = e.recorder.RecordStep(ctx, runID, target, step.Name, action,
		step.Args, output, sr.Duration, sr.Error)

	// Record approval requirement (best-effort, non-blocking in MVP).
	if step.Approval != nil {
		_, _ = e.recorder.RecordApproval(ctx, runID, step.Approval.Level,
			"", "required", "approval required for step "+step.Name)
	}

	// Record gate checks (best-effort, simulated pass in MVP).
	if step.Gate != nil {
		for _, c := range step.Gate.Pre {
			_, _ = e.recorder.RecordGate(ctx, runID, c.Command, "pre_apply", true, nil)
		}
		for _, c := range step.Gate.Post {
			_, _ = e.recorder.RecordGate(ctx, runID, c.Command, "post_apply", true, nil)
		}
	}

	return sr
}

// newRunID generates a 16-byte hex-encoded run id (32 chars). It uses
// crypto/rand so that ids are unpredictable and collision-resistant.
func newRunID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
