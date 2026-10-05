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
//     reverses the executed batches — unless the run-level policy
//     (plan.Rollback.OnFailure) is "manual", in which case the automatic
//     rollback is suppressed and the failed run waits for an operator
//     (ClosureResult.ManualRollbackRequired). When a PostRollbackVerifier
//     (T037) is configured AND the plan opts in (plan.Rollback.VerifyAfter,
//     spec §7.1), post-rollback verification runs and the result is
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

	stderrors "errors"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/lock"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// --- Phase identifiers ------------------------------------------------------

// ErrFencedOut is the sentinel an execution-layer step callback returns
// when it has lost ownership of the run (execution lease invalidated by a
// failover takeover, see docs/design-cluster-failover.md §7.3-3). It is
// deliberately NOT context cancellation and NOT a plain step error:
// once the closure has entered batch execution, every failure path —
// including ctx cancellation — triggers the rollback flow, and that flow
// deliberately runs detached to completion (see the rollback section of
// Run). A fenced-out executor must therefore fail WITHOUT rolling back:
// its undo commands are exactly the remote double-write fencing exists
// to prevent. The engine package defines its own sentinel (rather than
// importing cluster's) so the closure stays dependency-free; the wiring
// layer wraps cluster.ErrFencedOut into this one and Run recognises it
// via errors.Is, including through the fmt.Errorf("%…w") wrappers the
// batch controller produces.
var ErrFencedOut = stderrors.New("engine: execution fenced out")

// ClosurePhase identifies the outcome phase of a closure run. It is the
// stable string carried in ClosureResult.Phase.
type ClosurePhase string

const (
	// PhaseCompleted indicates the closure ran to completion: every batch
	// succeeded and every verification gate passed.
	PhaseCompleted ClosurePhase = "completed"

	// PhaseRolledBack indicates a verification failure triggered rollback
	// and the rollback fully restored what had been applied: every executed
	// step that needed compensation got it, and no step's side effects were
	// left undetermined (D-2 v2 verdict).
	//
	// The three rollback phases deliberately carry the SAME string values as
	// the corresponding run statuses (runstatus.StatusRolledBack etc.):
	// ClosureResult.Phase is what settleRun maps into run.status, so a
	// divergence here would silently produce a status the UI has no label for.
	// runClosurePhases (below) pins the correspondence in a test.
	PhaseRolledBack ClosurePhase = "rolled_back"

	// PhasePartialRollback indicates the rollback ran but did NOT fully
	// restore the run: some required compensations were skipped or failed
	// while others completed (D-2 v2). Deliberately NOT reported as
	// rolled_back — claiming a clean rollback would be a statement the
	// evidence does not support. Operators must inspect the host state.
	PhasePartialRollback ClosurePhase = "rolled_back_partial"

	// PhaseRollbackIncomplete indicates executed work needed compensation but
	// nothing could be restored (no compensation completed), or the executed
	// steps left undetermined side effects (D-2 v2). Treat as an incident
	// requiring human remediation, not as a settled rollback.
	PhaseRollbackIncomplete ClosurePhase = "rollback_incomplete"

	// PhaseFailed indicates the closure could not complete cleanly. This
	// covers pre-apply gate failure, lock conflict, and batch execution error
	// without rollback.
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
	// failed rollback with Phase == PhaseFailed). It stays nil when the
	// run-level policy suppressed the rollback
	// (ManualRollbackRequired == true).
	RollbackResult *rollback.RollbackResult

	// ManualRollbackRequired is true when the run-level rollback policy
	// (plan.Rollback.OnFailure == "manual", spec §7.1) suppressed the
	// automatic rollback: the run failed with its applied batches left in
	// place and waits for an operator to trigger the rollback
	// (RollbackChange / `levee rollback`). False on every other path,
	// including a rollback that ran and failed.
	ManualRollbackRequired bool

	// PostVerifyResult is the outcome of the post-rollback verification
	// (T037). It is non-nil only when a PostRollbackVerifier is
	// configured, rollback was triggered, and the plan opted in with
	// rollback.verify_after (see postRollbackVerifyRequested). A run that
	// never reached the rollback path leaves it nil.
	PostVerifyResult *rollback.PostVerifyResult

	// Error is the first error encountered during the closure, or nil
	// when Phase == PhaseCompleted. It is preserved separately from the
	// per-phase errors so that callers can use errors.Is / errors.As on
	// the top-level result.
	Error error

	// RunSnapshotRestoreError is non-nil when the run-level baseline could
	// not be written back during a rollback.
	//
	// It is a separate field rather than an append to Error on purpose. Error
	// carries the run's verdict, and the verdict here is already decided by
	// the compensation ledger: a run whose compensations all completed was
	// rolled back, and appending "and also the baseline restore failed" would
	// blur two different facts into one string that callers string-match on.
	// This one is the operator's "your files may not be back" signal, so it
	// gets its own field and is logged at error level.
	RunSnapshotRestoreError error
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

	// gateRuntime carries the dependencies parameterised verification gates
	// need (Prometheus endpoint for slo gates, approver transport for human
	// gates). It is supplied via WithGateRuntime; the zero value materialises
	// plans whose declarations need no runtime, and any slo/human declaration
	// against it fails materialisation with an explicit error (fail-closed).
	gateRuntime GateRuntime

	// hostGuard is the optional pre-execution policy hook installed via
	// WithHostGuard. It runs after target collection and before any lock is
	// acquired; a non-nil error aborts the run with PhaseFailed.
	hostGuard func(ctx context.Context, hosts []string) error

	// snapshotter is the optional pre-apply snapshot coordinator installed
	// via WithSnapshotter. It runs after lock acquisition and before batch
	// execution; a capture failure aborts the run (no mutation). Nil means
	// snapshot capture/restore is disabled (the pre-wiring no-op).
	snapshotter Snapshotter

	// runSnapshotter is the run-level baseline coordinator (see
	// snapshot_hook.go). Kept as a separate field from snapshotter on
	// purpose: the step-level hook is a no-op when absent, this one is
	// fail-closed, because a declared baseline with nothing to record it
	// would leave the operator believing rollback can restore a pre-state
	// that was never captured.
	runSnapshotter RunSnapshotter
}

// ClosureOption configures optional ClosureRunner behaviour at construction
// time.
type ClosureOption func(*ClosureRunner)

// WithGateRuntime supplies the runtime dependencies that parameterised
// verification gates require: GateRuntime.PrometheusURL backs slo checks and
// GateRuntime.Approver backs human checks. Omitting it is fine for plans
// that only declare cmd/probe gates; a plan that declares slo/human gates
// without the corresponding dependency fails at gate materialisation with an
// explicit error naming the missing configuration.
func WithGateRuntime(rt GateRuntime) ClosureOption {
	return func(cr *ClosureRunner) { cr.gateRuntime = rt }
}

// WithHostGuard installs a pre-execution policy hook. It runs AFTER the
// plan's target set is collected and BEFORE any lock is acquired; a
// non-nil error aborts the run with PhaseFailed and no locks taken. This
// is the enforcement point for the apply-time frozen-target re-check
// (inventory.ValidateNotFrozen): planning rejects frozen hosts up front,
// and this guard catches hosts that were frozen between planning and
// execution. Omitting it disables the second check.
func WithHostGuard(guard func(ctx context.Context, hosts []string) error) ClosureOption {
	return func(cr *ClosureRunner) { cr.hostGuard = guard }
}

// NewClosureRunner returns a ClosureRunner wired to the given subsystems.
// All required arguments must be non-nil; postVerifier is optional (pass
// nil to disable post-rollback verification). Additional options may supply
// runtime dependencies for parameterised verification gates (see
// WithGateRuntime).
//
// Configuring a postVerifier does not by itself verify anything: each plan
// decides for itself through rollback.verify_after (spec §7.1, opt-in — see
// postRollbackVerifyRequested). Wiring a verifier into a deployment therefore
// changes nothing for plans that do not ask for it.
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
	opts ...ClosureOption,
) *ClosureRunner {
	cr := &ClosureRunner{
		store:        store,
		lockManager:  lockManager,
		verifier:     verifier,
		rollback:     rollbackMgr,
		batchCtrl:    batchCtrl,
		postVerifier: postVerifier,
	}
	for _, opt := range opts {
		opt(cr)
	}
	return cr
}

// rollbackPolicyOf resolves the plan's run-level failure policy
// (plan.Rollback.OnFailure, spec §7.1). A nil plan or an absent/unknown
// value resolves to auto — see dsl.ResolveRollbackOnFailure for why unknown
// values keep the historical behaviour.
func rollbackPolicyOf(p *plan.Plan) string {
	if p == nil {
		return dsl.RollbackOnFailureAuto
	}
	return dsl.ResolveRollbackOnFailure(p.Rollback)
}

// postRollbackVerifyRequested resolves the plan's run-level "verify after
// rollback" policy (plan.Rollback.VerifyAfter, spec §7.1).
//
// The policy is opt-in: absent — a nil plan, a nil spec, or the false the
// parser yields for an omitted verify_after — means "do not verify", and only
// an explicit verify_after: true turns it on. Spec §7.1 formerly stated the
// opposite default ("缺省 true"), which no code ever implemented: the verifier
// existed but nothing was wired to it, so the field changed nothing either
// way. Opt-in is the same call rollbackPolicyOf makes for pre-gate legacy
// values — activating post-rollback gates for every plan written before the
// feature existed would start dispatching gate commands at targets on
// upgrade, and that is a new side effect on the compensation path rather than
// a bug fix. Plans that want the check declare it; spec §7.1 and
// docs/product-roadmap.md record the decision.
func postRollbackVerifyRequested(p *plan.Plan) bool {
	if p == nil || p.Rollback == nil {
		return false
	}
	return p.Rollback.VerifyAfter
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
// The returned *ClosureResult is always non-nil (the only exception being
// an extraordinary crypto/rand failure while minting the run id, which
// returns a nil result and an error). The returned error is
// non-nil only for fatal pre-conditions (nil plan, pre-apply gate
// failure, lock conflict, context cancellation before execution). When
// rollback is triggered, the error is nil and the outcome is carried in
// the result's Phase and RollbackResult fields.
func (cr *ClosureRunner) Run(ctx context.Context, p *plan.Plan, execFn rollback.ExecuteFunc) (*ClosureResult, error) {
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	result := &ClosureResult{
		RunID: runID,
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

	// 0. Pre-execution host policy (apply-time frozen re-check). Runs before
	//    gates, locks and any mutation: a rejected host aborts the run
	//    cleanly with PhaseFailed and no side effects.
	if cr.hostGuard != nil {
		if err := cr.hostGuard(ctx, targets); err != nil {
			result.Phase = PhaseFailed
			result.Error = fmt.Errorf("closure: host guard: %w", err)
			return result, result.Error
		}
	}

	// 1. Materialise inline gate declarations (plan.PlanStep.Gate) into
	//    registered verify.Gate implementations so the phase runs below
	//    actually execute what the workflow declared. Declarations the
	//    engine cannot execute — unknown check types, invalid params, or
	//    slo/human gates whose runtime dependency (Prometheus URL, approver)
	//    was not supplied via WithGateRuntime — fail materialisation: a
	//    declared gate must never silently pass. Command gates execute
	//    against GateInput.Channel; when no channel is supplied they report
	//    Passed=false ("missing channel"), which fails the phase honestly
	//    rather than fabricating a pass.
	if err := materializeStepGates(cr.verifier, p, cr.gateRuntime); err != nil {
		result.Phase = PhaseFailed
		result.Error = fmt.Errorf("closure: materialise gates: %w", err)
		return result, result.Error
	}
	preInput := verify.GateInput{

		ChannelFor: cr.gateRuntime.Channels, RunID: result.RunID,
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

	// 2.5 Pre-apply snapshot capture. Both halves live in one helper: the
	// step-level capture (per target/step, no-op when not installed) and the
	// run-level baseline (once per run, fail-closed). They belong together —
	// same position in the flow (all locks held, zero mutations yet), same
	// abort semantics — and folding them into one call keeps Run's
	// complexity budget for the branches that actually differ.
	if err := cr.captureSnapshotsAndBaseline(ctx, result.RunID, p, targets); err != nil {
		result.Phase = PhaseFailed
		result.Error = err
		return result, err
	}
	// 3. Batch execution. Batches run sequentially; after each batch we
	// run the post-batch gates. A batch error or gate failure stops
	// further batches and triggers rollback — EXCEPT when the failure is
	// ErrFencedOut: losing ownership of the run (failover takeover) is
	// not a change failure, and rolling back would dispatch undo commands
	// from an executor the cluster just invalidated — the exact remote
	// double-write fencing exists to prevent. We track which batches
	// were actually executed so rollback only reverses applied work.
	batchExecFn := adaptExecFunc(execFn)
	var triggerRollback bool
	var rollbackReason string
	var fencedOut bool
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
			ID:            p.ID,
			WorkflowName:  p.WorkflowName,
			Batches:       []plan.Batch{b},
			TotalTargets:  len(b.Targets),
			CreatedAt:     p.CreatedAt,
			RiskScore:     p.RiskScore,
			RiskFactors:   p.RiskFactors,
			ApprovalFloor: p.ApprovalFloor,
			Approval:      p.Approval,
			Rollback:      p.Rollback,
			Gate:          p.Gate,
		}
		brs := cr.batchCtrl.Execute(ctx, subPlan, batchExecFn)
		result.BatchResults = append(result.BatchResults, brs...)
		executedBatches = append(executedBatches, b)

		// A batch error aborts further batches and triggers rollback —
		// unless the error is the fencing sentinel (skip-rollback above).
		if len(brs) > 0 && brs[len(brs)-1].Error != nil {
			lastErr := brs[len(brs)-1].Error
			if stderrors.Is(lastErr, ErrFencedOut) {
				fencedOut = true
				rollbackReason = fmt.Sprintf("execution fenced out during batch %d", b.Index)
				break
			}
			triggerRollback = true
			rollbackReason = fmt.Sprintf("batch %d execution failed: %v", b.Index, lastErr)
			break
		}

		// Post-batch verification.
		postBatchInput := verify.GateInput{

			ChannelFor: cr.gateRuntime.Channels, RunID: result.RunID,
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

				ChannelFor: cr.gateRuntime.Channels, RunID: result.RunID,
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
	// rollback, reverse only the executed batches via rollback.Manager.
	// Then, when a PostRollbackVerifier is configured, run the
	// post-rollback verification (T037).
	//
	// Fenced-out executions never reach here: they fail fast below
	// without rolling back (see the batch-loop comment) — the undo
	// dispatches of a superseded executor are the double-write the
	// fencing design prohibits.
	if fencedOut {
		result.Phase = PhaseFailed
		result.Error = fmt.Errorf("closure: %s; not rolling back: %w", rollbackReason, ErrFencedOut)
		return result, result.Error
	}
	if triggerRollback {
		// Run-level policy (plan.Rollback.OnFailure, spec §7.1). "manual"
		// suppresses the automatic compensation: the failed run keeps its
		// applied batches and waits for an operator-triggered rollback
		// (RollbackChange / `levee rollback`). on_failure is one of only
		// two fields a workflow-level rollback block may carry — LE097
		// rejects compensation content there, because the ledger attributes
		// compensations per (host, forward step).
		if rollbackPolicyOf(p) == dsl.RollbackOnFailureManual {
			result.Phase = PhaseFailed
			result.ManualRollbackRequired = true
			result.Error = fmt.Errorf("closure: %s; on_failure=manual: automatic rollback suppressed, operator-triggered rollback required (applied batches kept)", rollbackReason)
			return result, result.Error
		}
		// Build a sub-plan containing only the batches that were actually
		// executed, so rollback does not try to undo work that never
		// started.
		executedPlan := &plan.Plan{
			ID:            p.ID,
			WorkflowName:  p.WorkflowName,
			Batches:       executedBatches,
			TotalTargets:  countTargets(executedBatches),
			CreatedAt:     p.CreatedAt,
			RiskScore:     p.RiskScore,
			RiskFactors:   p.RiskFactors,
			ApprovalFloor: p.ApprovalFloor,
			Approval:      p.Approval,
			Rollback:      p.Rollback,
			Gate:          p.Gate,
		}
		// Deliberately detached from ctx: cancellation is itself one of
		// the rollback triggers, so a rollback must run to completion
		// even when the caller's context is already cancelled or about
		// to be. Interrupting it with the caller's ctx would leave
		// applied batches unwound. Trade-off: rollback cannot be
		// cancelled once triggered; it relies on per-step execution
		// timeouts for bounded runtime.
		//
		// The run id keys snapshot lookup for strategy-"snapshot" steps
		// (SetRunID right before the flow — the Manager was constructed
		// before the run id existed).
		cr.rollback.SetRunID(result.RunID)
		// D-2 v2: compensate exactly what the apply evidence says actually
		// ran. result.BatchResults accumulates every batch outcome
		// (including the failed batch's partial results), so steps never
		// dispatched are not compensated; steps whose forward execution
		// failed are compensated and counted in UnknownSideEffects —
		// surfaced below for operator inspection, informational per
		// design item 3.
		ledger := rollback.LedgerFromBatchResults(result.BatchResults)
		rbResult := cr.rollback.RollbackWithLedger(context.Background(), executedPlan, execFn, ledger)
		result.RollbackResult = rbResult

		// Run-level baseline restore. Placed AFTER the compensation walk so
		// that the run's pre-state wins for the paths it covers: step
		// compensations and step snapshot restores handle their own paths
		// first, and the baseline — captured before the first batch — is the
		// last word on the run-level ones.
		cr.restoreRunBaseline(result, p, targets)

		// Post-rollback verification (T037): needs both a configured
		// verifier and a plan that asked for it. verify_after is opt-in
		// (postRollbackVerifyRequested explains why the default is off), so
		// wiring a verifier into a deployment does not change what plans
		// that never declared it execute.
		//
		// Cancellation is handled inside the verifier, not here: the gates
		// and the grade-action dispatch run on a context detached from ctx
		// but bounded by WithVerifyTimeout. Cancellation is one of the three
		// ways a rollback gets triggered, so inheriting it here would mean
		// the most interesting case — "operator hit Ctrl-C, did the undo
		// leave the system healthy?" — is the one case that never gets
		// answered. The bound is what makes continuing safe.
		if cr.postVerifier != nil && postRollbackVerifyRequested(p) {
			pvInput := verify.GateInput{

				ChannelFor: cr.gateRuntime.Channels, RunID: result.RunID,
				TargetIDs: targets,
			}
			// VerifyAndGrade, not Verify: identical verification, plus the
			// grade classification and the notify / escalate / audit
			// dispatch that the wiring-attached Grader prescribes. With a
			// nil Grader the two are equivalent, so this costs nothing for
			// deployments that have not wired one.
			//
			// A dispatch failure is logged, never fatal. The run's fate was
			// already decided by the rollback above; a webhook that refuses
			// the payload must not rewrite a completed rollback into a
			// failed one, and PostVerifyResult is not part of any run-status
			// mapping.
			pv, dispatchErr := cr.postVerifier.VerifyAndGrade(ctx, rbResult, nil, pvInput)
			result.PostVerifyResult = pv
			if dispatchErr != nil {
				log.Warn("post-rollback verify: grade action dispatch failed",
					"run_id", result.RunID,
					"error", dispatchErr)
			}
		}

		if rbResult.Success {
			result.Phase = PhaseRolledBack
			// Surface the reason that triggered the rollback as the
			// result error so callers can inspect it, even though the
			// rollback itself succeeded.
			result.Error = fmt.Errorf("closure: %s", rollbackReason)
			return result, nil
		}
		// Not a clean rollback (D-2 v2): distinguish "some state restored,
		// some not" from "nothing restored" so operators and status
		// mapping never mistake an uncompensated gap for a settled
		// rollback. The counts come from the compensation bookkeeping,
		// not from whether individual commands happened to error;
		// undetermined forward side effects are reported alongside.
		verdict := fmt.Sprintf("compensations %d/%d completed, %d forward steps with undetermined side effects",
			rbResult.CompletedCompensations, rbResult.RequiredCompensations, rbResult.UnknownSideEffects)
		if rbResult.PartialRollback {
			result.Phase = PhasePartialRollback
		} else {
			result.Phase = PhaseRollbackIncomplete
		}
		if rbResult.Error != nil {
			result.Error = fmt.Errorf("closure: %s; rollback incomplete (%s): %w", rollbackReason, verdict, rbResult.Error)
		} else {
			result.Error = fmt.Errorf("closure: %s; rollback incomplete (%s)", rollbackReason, verdict)
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

// newRunID generates a unique run identifier of the form "run-<16-hex-chars>".
// Run IDs key the execution record and the audit chain, so a crypto/rand
// failure is an error — no timestamp fallback (SA-012).
func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("engine: generate run id: %w", err)
	}
	return "run-" + hex.EncodeToString(b), nil
}
