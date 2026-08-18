package autoplanner

// auto_executor.go implements the AutoExecutor that drives the full
// auto-apply lifecycle: plan -> assess -> execute -> (rollback on failure).
//
// The executor bridges the AutoPlanner (which produces a formal Workflow) and
// the WorkflowExecutor interface (which applies the workflow to the target
// environment). The risk policy is delegated to RiskAssessor so that the
// decision of whether a workflow may run unattended stays single-sourced.
//
// Execution modes:
//
//	ModeDryRun  - preview only, no side effects.
//	ModeAuto    - low risk runs automatically; medium / high / critical risk
//	              require human approval (ErrRequiresApproval).
//	ModeForce   - run regardless of risk; the caller must hold the elevated
//	              permission that allows forced execution.
//
// On execution failure the AutoExecutor automatically triggers a rollback.
// When the rollback also fails the error is escalated to ErrRollbackFailed so
// that the operator can raise an alert.
//
// The AutoExecutor is safe for concurrent use: all fields are read-only after
// construction. It never panics; errors are propagated through error returns.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrExecNilRecommendation is returned when Execute is called with a nil
	// recommendation. The name is prefixed with "Exec" to avoid clashing with
	// ErrNilRecommendation defined in planner.go.
	ErrExecNilRecommendation = errors.New("auto_executor: nil recommendation")
	// ErrExecNilWorkflow is returned when ExecuteWorkflow is called with a
	// nil workflow.
	ErrExecNilWorkflow = errors.New("auto_executor: nil workflow")
	// ErrRequiresApproval is returned when ModeAuto is used for a workflow
	// whose risk level requires explicit human approval.
	ErrRequiresApproval = errors.New("auto_executor: requires human approval")
	// ErrRollbackFailed is returned when both execution and rollback fail.
	// This is the escalation signal for the operator.
	ErrRollbackFailed = errors.New("auto_executor: rollback failed")
	// ErrNilExecutor is returned when the AutoExecutor is used without a
	// configured WorkflowExecutor.
	ErrNilExecutor = errors.New("auto_executor: nil executor")
	// ErrExecutionFailed is returned when the workflow execution fails but
	// the rollback succeeds. The original execution error is wrapped inside.
	ErrExecutionFailed = errors.New("auto_executor: execution failed")
	// ErrUnknownExecutionMode is returned when ExecuteWorkflow is called with
	// an ExecutionMode value that is not one of the defined constants.
	ErrUnknownExecutionMode = errors.New("auto_executor: unknown execution mode")
)

// --- ExecutionMode ----------------------------------------------------------

// ExecutionMode controls how the AutoExecutor runs a workflow.
type ExecutionMode int

const (
	// ModeDryRun previews the workflow without executing it. The returned
	// ExecutionResult has Success=true and StepsTotal populated; no side
	// effects are produced and the WorkflowExecutor is never invoked.
	ModeDryRun ExecutionMode = iota
	// ModeAuto executes low-risk workflows automatically and requires human
	// approval for medium / high / critical risk workflows.
	ModeAuto
	// ModeForce executes the workflow regardless of risk. The caller must
	// hold the elevated permission that allows forced execution. Failure
	// still triggers an automatic rollback.
	ModeForce
)

// --- BatchResult ------------------------------------------------------------

// BatchResult is the per-batch outcome of a workflow execution.
type BatchResult struct {
	// BatchID is the 1-based batch ordinal.
	BatchID int
	// Success reports whether the batch completed without error.
	Success bool
	// Duration is the wall-clock time spent executing the batch.
	Duration time.Duration
	// Targets is the list of target hosts the batch ran on.
	Targets []string
	// FailedTargets is the subset of Targets that failed.
	FailedTargets []string
	// Error is the batch error message, empty on success.
	Error string
}

// --- ExecutionResult --------------------------------------------------------

// ExecutionResult is the outcome of a workflow execution. It is returned by
// AutoExecutor.Execute / ExecuteWorkflow and by the WorkflowExecutor
// implementation.
type ExecutionResult struct {
	// WorkflowID is the ID of the executed workflow.
	WorkflowID string
	// Success reports whether the workflow completed without error.
	Success bool
	// Duration is the total wall-clock execution time.
	Duration time.Duration
	// StepsTotal is the total number of steps across all batches.
	StepsTotal int
	// StepsFailed is the number of steps that failed.
	StepsFailed int
	// RollbackUsed reports whether a rollback was attempted.
	RollbackUsed bool
	// Error is the top-level error message, empty on success.
	Error string
	// StartedAt is the execution start time (UTC).
	StartedAt time.Time
	// FinishedAt is the execution finish time (UTC).
	FinishedAt time.Time
	// BatchResults is the per-batch outcome list.
	BatchResults []BatchResult
}

// --- WorkflowExecutor -------------------------------------------------------

// WorkflowExecutor is the interface that executes a workflow. It is abstracted
// so that tests can substitute a mock implementation and so that the apply
// phase can be swapped without touching the AutoExecutor.
type WorkflowExecutor interface {
	// Execute applies the workflow and returns the execution result.
	Execute(ctx context.Context, wf *Workflow) (*ExecutionResult, error)
	// Rollback reverses the effects of a previously executed workflow.
	Rollback(ctx context.Context, wf *Workflow) (*ExecutionResult, error)
}

// --- AutoExecutorConfig -----------------------------------------------------

// AutoExecutorConfig configures an AutoExecutor. Nil Planner / Assessor /
// Logger fields are replaced with sensible defaults by NewAutoExecutor.
// Executor is required; if nil, Execute / ExecuteWorkflow return
// ErrNilExecutor so that misconfiguration is detected at use time rather than
// construction time.
type AutoExecutorConfig struct {
	// Planner is the AutoPlanner. Nil -> NewAutoPlanner with zero config.
	Planner *AutoPlanner
	// Assessor is the RiskAssessor. Nil -> NewRiskAssessor.
	Assessor *RiskAssessor
	// Executor is the WorkflowExecutor. Required; if nil, the AutoExecutor
	// returns ErrNilExecutor from Execute / ExecuteWorkflow.
	Executor WorkflowExecutor
	// Logger is the structured logger. Nil -> log.With("component", ...).
	Logger *slog.Logger
}

// --- AutoExecutor -----------------------------------------------------------

// AutoExecutor drives the full auto-apply lifecycle: plan -> assess ->
// execute -> (rollback on failure). It is safe for concurrent use because all
// fields are read-only after construction.
type AutoExecutor struct {
	planner  *AutoPlanner
	assessor *RiskAssessor
	executor WorkflowExecutor
	log      *slog.Logger
}

// NewAutoExecutor returns an AutoExecutor with the given config. Nil Planner /
// Assessor / Logger fields are replaced with sensible defaults. A nil Executor
// is retained; the returned AutoExecutor will return ErrNilExecutor from
// Execute / ExecuteWorkflow so that misconfiguration is detected at use time
// rather than construction time.
func NewAutoExecutor(cfg AutoExecutorConfig) *AutoExecutor {
	planner := cfg.Planner
	if planner == nil {
		planner = NewAutoPlanner(AutoPlannerConfig{})
	}
	assessor := cfg.Assessor
	if assessor == nil {
		assessor = NewRiskAssessor()
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "auto_executor")
	}
	return &AutoExecutor{
		planner:  planner,
		assessor: assessor,
		executor: cfg.Executor,
		log:      lg,
	}
}

// Execute plans a workflow from rec and then executes it according to mode.
// It is the convenience entry point that chains AutoPlanner.Plan and
// ExecuteWorkflow. A nil recommendation yields ErrExecNilRecommendation; a
// planning failure is wrapped with "auto_executor: plan failed".
func (e *AutoExecutor) Execute(ctx context.Context, rec *recommend.Recommendation, mode ExecutionMode) (*ExecutionResult, error) {
	if rec == nil {
		return nil, ErrExecNilRecommendation
	}
	if ctx == nil {
		ctx = context.Background()
	}
	wf, err := e.planner.Plan(ctx, rec)
	if err != nil {
		return nil, fmt.Errorf("auto_executor: plan failed: %w", err)
	}
	return e.ExecuteWorkflow(ctx, wf, mode)
}

// ExecuteWorkflow executes wf according to mode. See ExecutionMode for the
// semantics of each mode. A nil workflow yields ErrExecNilWorkflow; a nil
// executor yields ErrNilExecutor.
func (e *AutoExecutor) ExecuteWorkflow(ctx context.Context, wf *Workflow, mode ExecutionMode) (*ExecutionResult, error) {
	if wf == nil {
		return nil, ErrExecNilWorkflow
	}
	if e.executor == nil {
		return nil, ErrNilExecutor
	}
	if ctx == nil {
		ctx = context.Background()
	}

	switch mode {
	case ModeDryRun:
		return e.dryRun(wf), nil
	case ModeAuto:
		return e.autoExecute(ctx, wf)
	case ModeForce:
		return e.forceExecute(ctx, wf)
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownExecutionMode, mode)
	}
}

// ShouldAutoExecute reports whether rec may be auto-executed without explicit
// human approval. It delegates to RiskAssessor.Assess and returns
// Assessment.CanAutoExecute. A nil recommendation yields false so that a nil
// pointer can never trigger an unattended change.
func (e *AutoExecutor) ShouldAutoExecute(rec *recommend.Recommendation) bool {
	if rec == nil {
		return false
	}
	return e.assessor.Assess(rec).CanAutoExecute
}

// --- internal helpers -------------------------------------------------------

// dryRun builds a preview ExecutionResult without invoking the executor. The
// result carries the total step count and a per-batch preview so that the
// operator can inspect what would have run.
func (e *AutoExecutor) dryRun(wf *Workflow) *ExecutionResult {
	now := time.Now().UTC()
	return &ExecutionResult{
		WorkflowID:   wf.ID,
		Success:      true,
		StepsTotal:   countSteps(wf),
		StartedAt:    now,
		FinishedAt:   now,
		BatchResults: buildBatchResults(wf, true),
	}
}

// autoExecute runs the assess-then-execute flow. When the assessment forbids
// auto-execution it returns ErrRequiresApproval. When execution fails it
// triggers a rollback; a rollback failure escalates to ErrRollbackFailed.
func (e *AutoExecutor) autoExecute(ctx context.Context, wf *Workflow) (*ExecutionResult, error) {
	assessment := e.assessWorkflow(wf)
	if !assessment.CanAutoExecute {
		e.log.InfoContext(ctx, "auto_executor: approval required",
			"workflow_id", wf.ID,
			"risk_level", string(wf.RiskLevel),
			"approval_level", assessment.ApprovalLevel,
		)
		now := time.Now().UTC()
		return &ExecutionResult{
			WorkflowID:   wf.ID,
			Success:      false,
			StepsTotal:   countSteps(wf),
			Error:        ErrRequiresApproval.Error(),
			StartedAt:    now,
			FinishedAt:   now,
			BatchResults: buildBatchResults(wf, false),
		}, ErrRequiresApproval
	}
	return e.executeWithRollback(ctx, wf)
}

// forceExecute runs the executor regardless of risk. Failure still triggers a
// rollback so that a forced change can never leave the system in a broken
// state without an attempted remediation.
func (e *AutoExecutor) forceExecute(ctx context.Context, wf *Workflow) (*ExecutionResult, error) {
	return e.executeWithRollback(ctx, wf)
}

// executeWithRollback runs the executor and, on failure, runs the rollback.
// A rollback failure escalates to ErrRollbackFailed; a successful rollback
// after a failed execution yields ErrExecutionFailed wrapping the original
// execution error.
func (e *AutoExecutor) executeWithRollback(ctx context.Context, wf *Workflow) (*ExecutionResult, error) {
	started := time.Now().UTC()
	res, err := e.executor.Execute(ctx, wf)
	if err == nil {
		finished := time.Now().UTC()
		if res == nil {
			res = &ExecutionResult{
				WorkflowID: wf.ID,
				Success:    true,
				StepsTotal: countSteps(wf),
				StartedAt:  started,
				FinishedAt: finished,
			}
		}
		if res.WorkflowID == "" {
			res.WorkflowID = wf.ID
		}
		if res.StartedAt.IsZero() {
			res.StartedAt = started
		}
		if res.FinishedAt.IsZero() {
			res.FinishedAt = finished
		}
		e.log.InfoContext(ctx, "auto_executor: execution succeeded",
			"workflow_id", wf.ID,
			"duration", res.Duration.String(),
		)
		return res, nil
	}

	// Execution failed: attempt rollback.
	e.log.WarnContext(ctx, "auto_executor: execution failed, rolling back",
		"workflow_id", wf.ID,
		"error", err.Error(),
	)
	rbRes, rbErr := e.executor.Rollback(ctx, wf)
	finished := time.Now().UTC()

	if rbErr != nil {
		// Rollback also failed: escalate.
		if rbRes == nil {
			rbRes = &ExecutionResult{
				WorkflowID: wf.ID,
				Success:    false,
				StartedAt:  started,
				FinishedAt: finished,
			}
		}
		rbRes.RollbackUsed = true
		rbRes.Success = false
		if rbRes.WorkflowID == "" {
			rbRes.WorkflowID = wf.ID
		}
		if rbRes.Error == "" {
			rbRes.Error = fmt.Sprintf("execution: %v; rollback: %v", err, rbErr)
		}
		return rbRes, fmt.Errorf("%w: %v", ErrRollbackFailed, rbErr)
	}

	// Rollback succeeded: report the original execution failure.
	if res == nil {
		res = &ExecutionResult{
			WorkflowID: wf.ID,
			StartedAt:  started,
			FinishedAt: finished,
		}
	}
	res.Success = false
	res.RollbackUsed = true
	res.StartedAt = started
	res.FinishedAt = finished
	if res.WorkflowID == "" {
		res.WorkflowID = wf.ID
	}
	if res.Error == "" {
		res.Error = err.Error()
	}
	return res, fmt.Errorf("%w: %v", ErrExecutionFailed, err)
}

// assessWorkflow maps a workflow to an Assessment by synthesising a
// recommendation from the workflow's risk level and target. This keeps the
// risk policy single-sourced in RiskAssessor without requiring a separate
// AssessWorkflow method on RiskAssessor.
func (e *AutoExecutor) assessWorkflow(wf *Workflow) Assessment {
	return e.assessor.Assess(&recommend.Recommendation{
		RiskLevel: wf.RiskLevel,
		Target:    wf.Target,
	})
}

// countSteps returns the total number of steps across all batches of wf.
func countSteps(wf *Workflow) int {
	total := 0
	for i := range wf.Batches {
		total += len(wf.Batches[i].Steps)
	}
	return total
}

// buildBatchResults builds a BatchResult list for wf. When success is true all
// batches are marked successful; otherwise all are marked failed with their
// targets listed in FailedTargets. This is used for dry-run previews and
// fallback results. A workflow with no batches returns nil.
func buildBatchResults(wf *Workflow, success bool) []BatchResult {
	if len(wf.Batches) == 0 {
		return nil
	}
	out := make([]BatchResult, len(wf.Batches))
	for i, b := range wf.Batches {
		out[i] = BatchResult{
			BatchID: b.ID,
			Success: success,
			Targets: b.Targets,
		}
		if !success {
			out[i].FailedTargets = b.Targets
			out[i].Error = "batch failed"
		}
	}
	return out
}
