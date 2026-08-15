// executor_test.go — unit tests for CompatExecutor (T056).
//
// The tests cover the 12 acceptance criteria from mvp-tasks.md T056:
// basic execution, per-module step results (shell/command/file/copy/template),
// multi-step and multi-target expansion, audit trace recording, empty
// workflow / no-targets errors, success aggregation, positive duration, and
// approval marking. Each test uses a fresh temp-file SQLite store so that the
// audit trace foreign-key constraint (trace.run_id → runs.id) is satisfied by
// the run row that Execute creates internally.

package compat

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test helpers ----------------------------------------------------------

// newTestStore opens a fresh SQLiteStore backed by a temp file. Each test gets
// its own file so tests can run in parallel without colliding.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "compat_exec_test.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newExecutor builds a CompatExecutor on top of a fresh temp-file store and
// a TraceRecorder backed by the same store. The executor creates run rows in
// the store, satisfying the trace foreign-key constraint.
func newExecutor(t *testing.T) *CompatExecutor {
	t.Helper()
	store := newTestStore(t)
	rec, err := audit.NewTraceRecorder(store)
	require.NoError(t, err)
	return NewCompatExecutor(store, rec)
}

// newWorkflow builds a dsl.Workflow with the given name and steps.
func newWorkflow(name string, steps ...dsl.Step) *dsl.Workflow {
	return &dsl.Workflow{
		Meta:  dsl.WorkflowMeta{Name: name},
		Steps: steps,
	}
}

// newStep builds a dsl.Step with empty args.
func newStep(name, module, action string) dsl.Step {
	return dsl.Step{
		Name:   name,
		Module: module,
		Action: action,
		Args:   map[string]any{},
	}
}

// findStep returns the first StepExecResult with the given target and step
// name, failing the test if none matches.
func findStep(t *testing.T, steps []StepExecResult, target, name string) StepExecResult {
	t.Helper()
	for _, s := range steps {
		if s.Target == target && s.Name == name {
			return s
		}
	}
	require.Failf(t, "step not found", "target=%q name=%q", target, name)
	return StepExecResult{}
}

// --- tests -----------------------------------------------------------------

// TestExecute_Basic verifies that Execute runs a single-step workflow against
// one target and returns a non-nil result with a populated run id (req #1).
func TestExecute_Basic(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-basic", newStep("s1", "shell", "exec"))
	wf.Steps[0].Args["cmd"] = "echo hello"

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Len(t, res.RunID, 32) // 16-byte hex
	assert.Equal(t, "wf-basic", res.WorkflowName)
	assert.Len(t, res.Steps, 1)
	assert.Equal(t, "host-1", res.Steps[0].Target)
	assert.Equal(t, "s1", res.Steps[0].Name)
}

// TestExecute_ShellStep verifies that a shell step is simulated as successful
// and its StepExecResult carries the module/action/target (req #2).
func TestExecute_ShellStep(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-shell", newStep("run-script", "shell", "exec"))
	wf.Steps[0].Args["cmd"] = "/opt/deploy.sh"

	res, err := exec.Execute(context.Background(), wf, []string{"node-1"})
	require.NoError(t, err)

	s := res.Steps[0]
	assert.Equal(t, "shell", s.Module)
	assert.Equal(t, "exec", s.Action)
	assert.Equal(t, "node-1", s.Target)
	assert.True(t, s.Success)
	assert.NoError(t, s.Error)
}

// TestExecute_CommandStep verifies that a command step is simulated as
// successful (req #3).
func TestExecute_CommandStep(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-cmd", newStep("run-cmd", "command", "exec"))
	wf.Steps[0].Args["cmd"] = "uptime"

	res, err := exec.Execute(context.Background(), wf, []string{"node-1"})
	require.NoError(t, err)

	s := res.Steps[0]
	assert.Equal(t, "command", s.Module)
	assert.True(t, s.Success)
	assert.NoError(t, s.Error)
}

// TestExecute_FileCopyTemplateSteps verifies that file, copy and template
// steps are all simulated as successful (req #4).
func TestExecute_FileCopyTemplateSteps(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-file-ops",
		newStep("ensure-file", "file", "manage"),
		newStep("push-config", "copy", "copy"),
		newStep("render-tpl", "template", "template"),
	)

	res, err := exec.Execute(context.Background(), wf, []string{"node-1"})
	require.NoError(t, err)
	require.Len(t, res.Steps, 3)

	for _, s := range res.Steps {
		assert.True(t, s.Success, "step %q (%s.%s) should succeed", s.Name, s.Module, s.Action)
		assert.NoError(t, s.Error)
	}

	assert.Equal(t, "file", res.Steps[0].Module)
	assert.Equal(t, "copy", res.Steps[1].Module)
	assert.Equal(t, "template", res.Steps[2].Module)
}

// TestExecute_MultipleSteps verifies that every step in a multi-step workflow
// is executed and appears in the result (req #5).
func TestExecute_MultipleSteps(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-multi",
		newStep("step-a", "shell", "exec"),
		newStep("step-b", "file", "manage"),
		newStep("step-c", "copy", "copy"),
	)

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)
	require.Len(t, res.Steps, 3)

	assert.Equal(t, "step-a", res.Steps[0].Name)
	assert.Equal(t, "step-b", res.Steps[1].Name)
	assert.Equal(t, "step-c", res.Steps[2].Name)
}

// TestExecute_MultipleTargets verifies that every target × step combination
// is executed, producing len(targets) × len(steps) results in target-major
// order (req #6).
func TestExecute_MultipleTargets(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-multi-target",
		newStep("s1", "shell", "exec"),
		newStep("s2", "file", "manage"),
	)
	targets := []string{"host-a", "host-b", "host-c"}

	res, err := exec.Execute(context.Background(), wf, targets)
	require.NoError(t, err)
	require.Len(t, res.Steps, 6) // 3 targets × 2 steps

	// Target-major order: host-a/s1, host-a/s2, host-b/s1, host-b/s2, ...
	assert.Equal(t, "host-a", res.Steps[0].Target)
	assert.Equal(t, "s1", res.Steps[0].Name)
	assert.Equal(t, "host-a", res.Steps[1].Target)
	assert.Equal(t, "s2", res.Steps[1].Name)
	assert.Equal(t, "host-b", res.Steps[2].Target)
	assert.Equal(t, "s1", res.Steps[2].Name)
	assert.Equal(t, "host-c", res.Steps[4].Target)
	assert.Equal(t, "s1", res.Steps[4].Name)

	// Every step on every target should succeed.
	for _, s := range res.Steps {
		assert.True(t, s.Success, "target=%q step=%q should succeed", s.Target, s.Name)
	}
}

// TestExecute_AuditTrace verifies that Execute records an audit trace per step
// via TraceRecorder.RecordStep, observable through ListByRun (req #7).
func TestExecute_AuditTrace(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-trace",
		newStep("traced-step", "shell", "exec"),
	)
	wf.Steps[0].Args["cmd"] = "echo traced"

	res, err := exec.Execute(context.Background(), wf, []string{"host-trace"})
	require.NoError(t, err)

	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)
	// At least one step_execute trace for the single step.
	require.NotEmpty(t, traces)
	foundStep := false
	for _, tr := range traces {
		if tr.Event == audit.EventStepExecute {
			foundStep = true
			assert.Equal(t, "system", tr.Actor)
		}
	}
	assert.True(t, foundStep, "expected a step_execute trace event")
}

// TestExecute_EmptyWorkflow verifies that a nil workflow or a workflow with
// no steps returns ErrEmptyWorkflow (req #8).
func TestExecute_EmptyWorkflow(t *testing.T) {
	exec := newExecutor(t)

	// nil workflow.
	_, err := exec.Execute(context.Background(), nil, []string{"host-1"})
	require.ErrorIs(t, err, ErrEmptyWorkflow)

	// Workflow with no steps.
	wf := newWorkflow("wf-empty")
	_, err = exec.Execute(context.Background(), wf, []string{"host-1"})
	require.ErrorIs(t, err, ErrEmptyWorkflow)
}

// TestExecute_NoTargets verifies that an empty target list returns
// ErrNoTargets (req #9).
func TestExecute_NoTargets(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-no-targets", newStep("s1", "shell", "exec"))

	_, err := exec.Execute(context.Background(), wf, nil)
	require.ErrorIs(t, err, ErrNoTargets)

	_, err = exec.Execute(context.Background(), wf, []string{})
	require.ErrorIs(t, err, ErrNoTargets)
}

// TestExecute_SuccessSummary verifies that Success is true and Error is nil
// when all steps succeed (req #10).
func TestExecute_SuccessSummary(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-success",
		newStep("s1", "shell", "exec"),
		newStep("s2", "file", "manage"),
	)

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	assert.True(t, res.Success)
	assert.NoError(t, res.Error)
}

// TestExecute_DurationPositive verifies that the total Duration is positive
// (req #11).
func TestExecute_DurationPositive(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-dur", newStep("s1", "shell", "exec"))

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	assert.True(t, res.Duration > 0, "duration should be positive, got %v", res.Duration)
}

// TestExecute_ApprovalRecorded verifies that a step with an ApprovalSpec
// produces an approval_decision audit trace (req #12).
func TestExecute_ApprovalRecorded(t *testing.T) {
	exec := newExecutor(t)
	step := newStep("needs-approval", "file", "manage")
	step.Approval = &dsl.ApprovalSpec{
		Level:        "high",
		MinApprovers: 2,
		Approvers:    []string{"alice", "bob"},
	}
	wf := newWorkflow("wf-approval", step)

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)

	foundApproval := false
	for _, tr := range traces {
		if tr.Event == audit.EventApprovalDecision {
			foundApproval = true
			// RecordApproval sets Actor to the approver; in MVP the approver
			// is empty (decision="required" means approval is pending).
			assert.Equal(t, "", tr.Actor)
		}
	}
	// The approval trace is recorded with Actor="" (no approver yet) and
	// decision="required". We just assert that an approval event exists.
	assert.True(t, foundApproval, "expected an approval_decision trace event")
}

// TestExecute_GateRecorded verifies that a step with a GateSpec produces
// gate_check audit traces for pre and post checks (supplementary to req #12).
func TestExecute_GateRecorded(t *testing.T) {
	exec := newExecutor(t)
	step := newStep("gated-step", "shell", "exec")
	step.Gate = &dsl.GateSpec{
		Pre:  []dsl.GateCheck{{Type: "cmd", Command: "pre-check.sh"}},
		Post: []dsl.GateCheck{{Type: "cmd", Command: "post-check.sh"}},
	}
	wf := newWorkflow("wf-gate", step)

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)

	gateCount := 0
	for _, tr := range traces {
		if tr.Event == audit.EventGateCheck {
			gateCount++
		}
	}
	assert.Equal(t, 2, gateCount, "expected 2 gate_check events (pre + post)")
}

// TestExecute_UnsupportedModuleFails verifies that a step using an unsupported
// module is marked as failed and propagates to the overall result.
func TestExecute_UnsupportedModuleFails(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-unsupported", newStep("bad-step", "pkg", "install"))

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err) // Execute itself does not error; the step fails.

	require.Len(t, res.Steps, 1)
	assert.False(t, res.Steps[0].Success)
	require.Error(t, res.Steps[0].Error)
	assert.ErrorIs(t, res.Steps[0].Error, ErrExecFailed)

	assert.False(t, res.Success)
	require.Error(t, res.Error)
	assert.ErrorIs(t, res.Error, ErrExecFailed)
}

// TestExecute_StepDurationPositive verifies that each step has a non-negative
// duration (sanity check on the per-step timing).
func TestExecute_StepDurationPositive(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-step-dur", newStep("s1", "shell", "exec"))

	res, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	for _, s := range res.Steps {
		assert.True(t, s.Duration >= 0, "step duration should be non-negative, got %v", s.Duration)
	}
}

// TestExecute_RunIDUnique verifies that two Execute calls produce different
// run ids.
func TestExecute_RunIDUnique(t *testing.T) {
	exec := newExecutor(t)
	wf := newWorkflow("wf-unique", newStep("s1", "shell", "exec"))

	res1, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)
	res2, err := exec.Execute(context.Background(), wf, []string{"host-1"})
	require.NoError(t, err)

	assert.NotEqual(t, res1.RunID, res2.RunID)
}

// TestNewCompatExecutor_ZeroValue verifies that NewCompatExecutor returns a
// non-nil executor (the zero value is not usable, but the constructor always
// returns a ready instance).
func TestNewCompatExecutor_ZeroValue(t *testing.T) {
	store := newTestStore(t)
	rec, err := audit.NewTraceRecorder(store)
	require.NoError(t, err)
	exec := NewCompatExecutor(store, rec)
	require.NotNil(t, exec)
}
