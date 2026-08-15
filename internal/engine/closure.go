// closure.go implements LEVEE's closed-loop change executor (MVP task T042).
// The ClosureRunner orchestrates the complete change lifecycle —
// plan → apply → verify → rollback — as a single atomic operation, wiring
// together the plan, batch, verify, lock and rollback subsystems that were
// built in earlier MVP tasks.
//
// Execution flow:
//
//  1. Pre-apply verification (verify.PhasePreApply). A failure aborts the
//     run before any change is made; no locks are acquired and no
//     rollback is needed.
//  2. Lock acquisition. Every target host in the plan is locked via
//     lock.LockManager. A lock conflict aborts the run; already-acquired
//     locks are released before returning.
//  3. Batch execution. Batches run sequentially via batch.Controller.
//     After each batch, verify.PhasePostBatch gates run; a failure stops
//     further batches and triggers rollback.
//  4. Post-apply verification (verify.PhasePostApply). A failure triggers
//     rollback of the whole run.
//  5. Rollback path. When any verification fails, rollback.Manager
//     reverses the executed batches. When a PostRollbackVerifier (T037)
//     is configured, post-rollback verification runs and the result is
//     recorded on the ClosureResult.
//  6. Lock release. Regardless of outcome, every acquired lock is
//     released before the ClosureRunner returns.
//
// The ClosureRunner is transport-agnostic: the caller supplies an
// rollback.ExecuteFunc that knows how to dispatch a dsl.Step to a target
// host. This keeps the closure free of channel / executor concerns and
// makes it trivially testable with a stub function.

package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/lock"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// --- Phase identifiers ------------------------------------------------------

// ClosurePhase identifies the outcome phase of a closure run. It is the
// stable string carried in ClosureResult.Phase.
type ClosurePhase string

const (
	// PhaseCompleted indicates the closure ran to completion: every batch
	// succeeded and every verification gate passed.
	PhaseCompleted ClosurePhase = "completed"

	// PhaseRolledBack indicates a verification failure triggered rollback
	// and the rollback succeeded (every rollback step completed without
	// error).
	PhaseRolledBack ClosurePhase = "rolled_back"

	// PhaseFailed indicates the closure could not complete cleanly. This
	// covers pre-apply gate failure, lock conflict, batch execution error
	// without rollback, and rollback failure (partial or total).
	PhaseFailed ClosurePhase = "failed"
)

// --- ClosureResult ----------------------------------------------------------

// ClosureResult is the outcome of a ClosureRunner.Run call. It is always
// non-nil, even when Run returns a non-nil error.
type ClosureResult struct {
	// RunID is the unique identifier of this closure run. It is generated
	// at the start of Run and used as the lock owner.
	RunID string

	// PlanID is the ID of the plan that was executed. It is empty when
	// Run is called with a nil plan.
	PlanID string

	// Phase is the outcome phase: "completed", "rolled_back" or "failed".
	Phase ClosurePhase

	// BatchResults holds the per-batch execution results, in plan order.
	// It is populated only for batches that were actually executed; a
	// pre-apply failure leaves it empty.
	BatchResults []*batch.BatchResult

	// VerifyResults holds the verification gate results collected during
	// the run, in the order they were executed (pre-apply, then
	// post-batch per executed batch, then post-apply).
	VerifyResults []verify.GateResult

	// RollbackResult is the outcome of the rollback flow. It is non-nil
	// only when rollback was triggered (Phase == PhaseRolledBack or a
	// failed rollback with Phase == PhaseFailed).
	RollbackResult *rollback.RollbackResult

	// PostVerifyResult is the outcome of the post-rollback verification
	// (T037). It is non-nil only when a PostRollbackVerifier is
	// configured and rollback was triggered.
	PostVerifyResult *rollback.PostVerifyResult

	// Error is the first error encountered during the closure, or nil
	// when Phase == PhaseCompleted. It is preserved separately from the
	// per-phase errors so that callers can use errors.Is / errors.As on
	// the top-level result.
	Error error
}

// --- ClosureRunner ----------------------------------------------------------

// ClosureRunner orchestrates the complete change closure:
// plan → apply → verify → rollback. It is the end-to-end coordinator of
// the LEVEE engine, wiring plan generation, batch execution, verification
// gates, lock management and rollback into a single atomic change
// operation.
//
// A zero-value ClosureRunner is not usable; create one with
// NewClosureRunner. A ClosureRunner is configured once at construction
// time and may then drive any number of plans sequentially. Driving
// plans concurrently from the same ClosureRunner is not supported.
type ClosureRunner struct {
	store        state.Store
	lockManager  *lock.LockManager
	verifier     *verify.GateManager
	rollback     *rollback.Manager
	batchCtrl    *batch.Controller
	postVerifier *rollback.PostRollbackVerifier // optional, T037
}

// NewClosureRunner returns a ClosureRunner wired to the given subsystems.
// All required arguments must be non-nil; postVerifier is optional (pass
// nil to disable post-rollback verification).
//
// The returned runner uses the lock manager's configured default TTL for
// target locks. Override it via lockManager.SetTTL before calling Run.
func NewClosureRunner(
	store state.Store,
	lockManager *lock.LockManager,
	verifier *verify.GateManager,
	rollbackMgr *rollback.Manager,
	batchCtrl *batch.Controller,
	postVerifier *rollback.PostRollbackVerifier,
) *ClosureRunner {
	return &ClosureRunner{
		store:        store,
		lockManager:  lockManager,
		verifier:     verifier,
		rollback:     rollbackMgr,
		batchCtrl:    batchCtrl,
		postVerifier: postVerifier,
	}
}

// --- Run --------------------------------------------------------------------

// Run executes the complete change closure for the given plan. It is the
// single entry point for the end-to-end change lifecycle.
//
// Parameters:
//   - ctx: context for cancellation and timeouts. When ctx is cancelled,
//     the runner stops at the next phase boundary, releases all acquired
//     locks and returns a result with Phase == PhaseFailed.
//   - p: the plan to execute. A nil plan produces a result with
//     Phase == PhaseFailed and an error, without acquiring locks or
//     running gates.
//   - execFn: the callback used to execute each step on each target. It
//     is adapted to batch.ExecuteFunc internally and passed to
//     rollback.Manager.Rollback for the rollback path. A nil execFn is
//     treated as a dry-run: batch execution is a no-op and rollback
//     records every step as skipped.
//
// The returned *ClosureResult is always non-nil. The returned error is
// non-nil only for fatal pre-conditions (nil plan, pre-apply gate
// failure, lock conflict, context cancellation before execution). When
// rollback is triggered, the error is nil and the outcome is carried in
// the result's Phase and RollbackResult fields.
func (cr *ClosureRunner) Run(ctx context.Context, p *plan.Plan, execFn rollback.ExecuteFunc) (*ClosureResult, error) {
	result := &ClosureResult{
		RunID: newRunID(),
		Phase: PhaseCompleted,
	}

	if p == nil {
		result.Phase = PhaseFailed
		result.Error = fmt.Errorf("closure: plan is nil")
		return result, result.Error
	}
	result.PlanID = p.ID

	// Collect the full target set (de-duplicated) for pre-apply gates
	// and lock acquisition.
	targets := collectTargets(p)

	// 1. Pre-apply verification. A failure aborts before any change is
	//    made: no locks, no apply, no rollback.
	preInput := verify.GateInput{
		RunID:     result.RunID,
		TargetIDs: targets,
	}
	preResults := cr.verifier.RunPhase(ctx, verify.PhasePreApply, preInput)
	result.VerifyResults = append(result.VerifyResults, preResults...)
	if !gatesPassed(preResults) {
		result.Phase = PhaseFailed
		result.Error = fmt.Errorf("closure: pre-apply gate failed")
		return result, result.Error
	}

	// Honour context cancellation before acquiring locks.
	if err := ctx.Err(); err != nil {
		result.Phase = PhaseFailed
		result.Error = fmt.Errorf("closure: cancelled before lock acquire: %w", err)
		return result, result.Error
	}

	// 2. Lock acquisition. Lock every target; on the first failure,
	//    release everything already held and abort.
	acquired := make(map[string]bool, len(targets))
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			cr.releaseLocks(context.Background(), result.RunID, acquired)
			result.Phase = PhaseFailed
			result.Error = fmt.Errorf("closure: cancelled during lock acquire: %w", err)
			return result, result.Error
		}
		if _, err := cr.lockManager.Acquire(ctx, target, result.RunID); err != nil {
			cr.releaseLocks(context.Background(), result.RunID, acquired)
			result.Phase = PhaseFailed
			result.Error = fmt.Errorf("closure: acquire lock for %s: %w", target, err)
			return result, result.Error
		}
		acquired[target] = true
	}
	// Ensure every acquired lock is released on return, regardless of
	// outcome. We use a background context for the deferred release so
	// that a cancelled ctx does not prevent lock cleanup.
	defer cr.releaseLocks(context.Background(), result.RunID, acquired)

	// 3. Batch execution. Batches run sequentially; after each batch we
	//    run the post-batch gates. A batch error or gate failure stops
	//    further batches and triggers rollback. We track which batches
	//    were actually executed so rollback only reverses applied work.
	batchExecFn := adaptExecFunc(execFn)
	var triggerRollback bool
	var rollbackReason string
	executedBatches := make([]plan.Batch, 0, len(p.Batches))

	for i, b := range p.Batches {
		if err := ctx.Err(); err != nil {
			triggerRollback = true
			rollbackReason = fmt.Errorf("cancelled before batch %d: %w", i, err).Error()
			break
		}

		// Execute the single batch via a one-batch sub-plan. This reuses
		// the batch.Controller's concurrency and error-policy logic
		// while giving us a clean boundary to insert post-batch gates.
		subPlan := &plan.Plan{
			ID:           p.ID,
			WorkflowName: p.WorkflowName,
			Batches:      []plan.Batch{b},
			TotalTargets: len(b.Targets),
			CreatedAt:    p.CreatedAt,
		}
		brs := cr.batchCtrl.Execute(ctx, subPlan, batchExecFn)
		result.BatchResults = append(result.BatchResults, brs...)
		executedBatches = append(executedBatches, b)

		// A batch error aborts further batches and triggers rollback.
		if len(brs) > 0 && brs[len(brs)-1].Error != nil {
			triggerRollback = true
			rollbackReason = fmt.Sprintf("batch %d execution failed: %v", b.Index, brs[len(brs)-1].Error)
			break
		}

		// Post-batch verification.
		postBatchInput := verify.GateInput{
			RunID:     result.RunID,
			BatchID:   fmt.Sprintf("batch-%d", b.Index),
			TargetIDs: b.Targets,
		}
		postBatchResults := cr.verifier.RunPhase(ctx, verify.PhasePostBatch, postBatchInput)
		result.VerifyResults = append(result.VerifyResults, postBatchResults...)
		if !gatesPassed(postBatchResults) {
			triggerRollback = true
			rollbackReason = fmt.Sprintf("post-batch gate failed after batch %d", b.Index)
			break
		}
	}

	// 4. Post-apply verification. Runs only when every batch succeeded
	//    and no rollback was triggered yet. A failure triggers rollback
	//    of the whole run.
	if !triggerRollback {
		if err := ctx.Err(); err != nil {
			triggerRollback = true
			rollbackReason = fmt.Errorf("cancelled before post-apply verify: %w", err).Error()
		} else {
			postInput := verify.GateInput{
				RunID:     result.RunID,
				TargetIDs: targets,
			}
			postResults := cr.verifier.RunPhase(ctx, verify.PhasePostApply, postInput)
			result.VerifyResults = append(result.VerifyResults, postResults...)
			if !gatesPassed(postResults) {
				triggerRollback = true
				rollbackReason = "post-apply gate failed"
			}
		}
	}

	// 5. Rollback path. When a verification or batch failure triggered
	//    rollback, reverse only the executed batches via rollback.Manager.
	//    Then, when a PostRollbackVerifier is configured, run the
	//    post-rollback verification (T037).
	if triggerRollback {
		// Build a sub-plan containing only the batches that were actually
		// executed, so rollback does not try to undo work that never
		// started.
		executedPlan := &plan.Plan{
			ID:           p.ID,
			WorkflowName: p.WorkflowName,
			Batches:      executedBatches,
			TotalTargets: countTargets(executedBatches),
			CreatedAt:    p.CreatedAt,
		}
		rbResult := cr.rollback.Rollback(ctx, executedPlan, execFn)
		result.RollbackResult = rbResult

		// Post-rollback verification (T037), when configured.
		if cr.postVerifier != nil {
			pvInput := verify.GateInput{
				RunID:     result.RunID,
				TargetIDs: targets,
			}
			result.PostVerifyResult = cr.postVerifier.Verify(ctx, rbResult, nil, pvInput)
		}

		if rbResult.Success {
			result.Phase = PhaseRolledBack
			// Surface the reason that triggered the rollback as the
			// result error so callers can inspect it, even though the
			// rollback itself succeeded.
			result.Error = fmt.Errorf("closure: %s", rollbackReason)
			return result, nil
		}
		// Rollback failed (partial or total).
		result.Phase = PhaseFailed
		if rbResult.Error != nil {
			result.Error = fmt.Errorf("closure: %s; rollback failed: %w", rollbackReason, rbResult.Error)
		} else {
			result.Error = fmt.Errorf("closure: %s; rollback failed", rollbackReason)
		}
		return result, nil
	}

	// 6. Success.
	result.Phase = PhaseCompleted
	return result, nil
}

// --- helpers ----------------------------------------------------------------

// adaptExecFunc converts a rollback.ExecuteFunc (which takes a dsl.Step)
// to a batch.ExecuteFunc (which takes a plan.Batch and plan.PlanStep).
// The batch and step metadata are mapped onto a dsl.Step so the same
// callback serves both the apply and rollback paths, keeping the
// caller's ExecuteFunc signature uniform.
func adaptExecFunc(execFn rollback.ExecuteFunc) batch.ExecuteFunc {
	return func(ctx context.Context, _ plan.Batch, target string, step plan.PlanStep) error {
		if execFn == nil {
			return nil
		}
		ds := dsl.Step{
			Name:     step.Name,
			Module:   step.Module,
			Action:   step.Action,
			Args:     step.Args,
			Rollback: step.Rollback,
			Approval: step.Approval,
			Gate:     step.Gate,
		}
		return execFn(ctx, target, ds)
	}
}

// collectTargets returns the de-duplicated list of all target hosts in
// the plan, preserving first-seen order. The result is used for pre-
// apply gates, lock acquisition and post-apply gates.
func collectTargets(p *plan.Plan) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, b := range p.Batches {
		for _, t := range b.Targets {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// countTargets returns the total number of target slots across all
// batches (with duplicates, matching plan.TotalTargets semantics). It is
// used to build the rollback sub-plan's TotalTargets field.
func countTargets(batches []plan.Batch) int {
	n := 0
	for _, b := range batches {
		n += len(b.Targets)
	}
	return n
}

// gatesPassed reports whether every gate result passed. An empty slice
// is considered a pass (no gates to fail).
func gatesPassed(results []verify.GateResult) bool {
	for _, r := range results {
		if !r.Passed {
			return false
		}
	}
	return true
}

// releaseLocks releases every lock in the acquired map. Errors are
// ignored because a failed release is not actionable at this point
// (the lock will expire via its TTL). A background-derived context is
// expected so that a cancelled run-time context does not block cleanup.
func (cr *ClosureRunner) releaseLocks(ctx context.Context, owner string, acquired map[string]bool) {
	for target := range acquired {
		_ = cr.lockManager.Release(ctx, target, owner)
	}
}

// newRunID generates a unique run identifier using crypto/rand. The ID
// has the form "run-<16-hex-chars>". On the extremely unlikely event
// that rand.Read fails, it falls back to a timestamp-based ID so the
// caller always gets a usable, unique-enough identifier.
func newRunID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b)
}
