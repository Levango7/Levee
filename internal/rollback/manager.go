package rollback

// Package rollback implements LEVEE's rollback protocol framework (design doc
// section 4.5, MVP task T035). It provides a Manager that, given a failed
// *plan.Plan, walks the executed batches in reverse order and invokes the
// rollback steps declared on each plan step's RollbackSpec.
//
// The framework is deliberately transport-agnostic: the Manager does not run
// modules itself. Instead it accepts an ExecuteFunc callback supplied by the
// apply phase, which knows how to dispatch a dsl.Step to a target host. This
// keeps the rollback package free of executor / channel concerns and makes it
// trivially testable with a stub function.
//
// Safety properties:
//
//   - Reverse batch order: the last batch executed is the first batch rolled
//     back, so that dependencies are undone in the correct order.
//   - Reverse step order within a batch: within a batch, steps are rolled
//     back in reverse execution order (last step first).
//   - Whitelist enforcement: a rollback step is executed only when its
//     module.action is allowed by the Manager's policy — WithWhitelistAll
//     allows everything, otherwise the action must be listed via WithWhitelist.
//     A denied step is skipped (not executed) and the denial is recorded in
//     the result so that operators can audit what was not undone.
//   - No-rollback passthrough: a plan step without a RollbackSpec is recorded
// as skipped with reason "no rollback spec" — it is not an error.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// --- ExecuteFunc -----------------------------------------------------------

// ExecuteFunc is the callback the Manager uses to run a single rollback step
// on a single target host. The caller (typically the apply phase) supplies
// this function; the Manager itself stays free of transport / executor
// concerns.
//
// The step is a dsl.Step taken from a RollbackSpec.Steps slice. The target is
// the host the original step ran on. The function must respect ctx for
// cancellation and timeouts. A non-nil error marks the rollback step as
// failed; the Manager decides whether to continue based on its stopOnError
// setting.
type ExecuteFunc func(ctx context.Context, target string, step dsl.Step) error

// --- results ---------------------------------------------------------------

// RollbackResult is the outcome of a full Manager.Rollback call. It aggregates
// per-batch results and a top-level success / partial-rollback verdict.
type RollbackResult struct {
	// BatchResults is the per-batch outcome slice, in the order the batches
	// were rolled back (reverse of execution order). It is always non-nil
	// when Rollback returns a non-nil result.
	BatchResults []BatchRollbackResult

	// Success reports whether the rollback fully restored the targets that
	// were actually changed. D-2 v2 verdict (design item 3):
	//   Success = Error == nil && RequiredCompensations == CompletedCompensations
	// i.e. no required compensation went missing (skipped: no spec, not
	// whitelisted, snapshot wiring absent) and no compensation command
	// failed. A rollback where nothing needed compensating is success
	// (Required 0). UnknownSideEffects does NOT affect this verdict — it is
	// the recorded extension point for the future side-effect-unknown state
	// (design D-2 v2 可持续性), surfaced for operator inspection instead.
	// With a NIL ledger the pre-D-2 compatibility reading holds (design
	// item 2): skips are benign, only command errors fail the verdict.
	Success bool

	// RequiredCompensations counts the original steps the execution ledger
	// records as run on their targets — each requires compensation. Steps
	// skipped as NotExecuted (forward step never dispatched) are excluded.
	RequiredCompensations int

	// CompletedCompensations counts those required compensations that ran to
	// completion without error.
	CompletedCompensations int

	// UnknownSideEffects counts steps that were dispatched but failed, so
	// whether they left residue is undetermined. Their declared compensation
	// is still attempted. Per the D-2 v2 design this count does not flip the
	// verdict: it is the deliberately-wired extension point for a future
	// side-effect-unknown state, carried here so callers (closure phases,
	// operator tooling) can surface it for inspection.
	UnknownSideEffects int

	// PartialRollback reports whether some compensation completed while the
	// verdict is still incomplete (Required > Completed: a required
	// compensation is missing or failed while others landed). Callers use it
	// to distinguish "rollback went half-way" from "nothing could be undone
	// at all".
	PartialRollback bool

	// Error is the first compensation error encountered, or nil when none
	// failed. NOTE: a nil Error with Success false means the gap was not a
	// failed command but missing compensation (no declared rollback, snapshot
	// restore not wired, whitelist denial) — callers must branch on Success,
	// not on Error.
	Error error

	// Duration is the wall-clock time spent inside Rollback, measured from
	// entry to the construction of the result.
	Duration time.Duration
}

// BatchRollbackResult is the outcome of rolling back a single batch.
type BatchRollbackResult struct {
	// BatchIndex is the 0-based batch ordinal in the original plan. Because
	// rollback runs in reverse order, BatchResults[0].BatchIndex is the
	// last batch in the plan, not batch 0.
	BatchIndex int

	// TargetResults is the per-target outcome slice. Targets within a batch
	// are rolled back concurrently up to the Manager's concurrency limit;
	// the slice order is the order targets were completed, which is not
	// deterministic under concurrency > 1.
	TargetResults []TargetRollbackResult
}

// TargetRollbackResult is the outcome of rolling back all steps that ran on a
// single target host within a batch.
type TargetRollbackResult struct {
	// Target is the host name.
	Target string

	// StepResults is the per-step outcome slice, in the order steps were
	// rolled back (reverse of execution order within the batch).
	StepResults []StepRollbackResult
}

// StepRollbackResult is the outcome of a single rollback step invocation.
type StepRollbackResult struct {
	// OrigStepName is the name of the original plan step whose RollbackSpec
	// produced this rollback step. It links the undo action back to the
	// forward action in the audit trail.
	OrigStepName string

	// RollbackStepName is the name of the rollback step itself (the Name
	// field of the dsl.Step inside RollbackSpec.Steps).
	RollbackStepName string

	// Module and Action identify the rollback operation, e.g. "pkg" /
	// "install" to undo a "pkg.remove".
	Module string
	Action string

	// Skipped reports whether the rollback step was not executed. When
	// true, SkipReason explains why and NotExecuted says whether the skip
	// is benign (the forward step never ran) or a gap (it ran but no
	// compensation could be applied). Skipped steps are never errors.
	Skipped bool

	// NotExecuted marks a skip whose cause is "the forward step never ran on
	// this target" (D-2 v2). Such skips are benign: there was nothing to undo,
	// so they do not count against the rollback verdict. A skip with
	// NotExecuted=false for a step that DID run is a compensation gap and
	// makes Success false.
	NotExecuted bool

	// SideEffectsUnknown marks a step that was dispatched but failed during
	// the forward apply, so whether it left a partial side effect is
	// undetermined. Its declared compensation is still attempted here, but
	// the flag propagates into RollbackResult.UnknownSideEffects so the run is
	// reported as not-fully-rolled-back and needs operator inspection.
	SideEffectsUnknown bool

	// SkipReason is a human-readable explanation when Skipped is true. It
	// is empty for executed steps.
	SkipReason string

	// Error is the error returned by ExecuteFunc for an executed (non-
	// skipped) step. It is nil on success or for skipped steps.
	Error error

	// Duration is the wall-clock time spent inside ExecuteFunc for an
	// executed step. It is zero for skipped steps.
	Duration time.Duration
}

// --- Manager ---------------------------------------------------------------

// Manager orchestrates rollback execution. It is configured at construction
// time via ManagerOption values and is safe for concurrent use: a single
// Manager may be reused across multiple Rollback calls (though in practice
// each failed apply gets its own Manager).
type Manager struct {
	whitelist    map[string]bool // "module.action" -> true
	whitelistAll bool            // escape hatch: allow every module.action
	concurrency  int             // per-batch target parallelism; >= 1
	stopOnError  bool            // stop at first step error

	// snapshotRestore is the optional strategy-"snapshot" restore callback
	// (see WithSnapshotRestore). runID keys the snapshot lookup (see
	// WithRunID). Both are supplied by the wiring layer; the engine passes
	// its closure run id when assembling the rollback manager.
	snapshotRestore SnapshotRestoreFunc
	runID           string
}

// ManagerOption configures a Manager at construction time.
type ManagerOption func(*Manager)

// WithWhitelist sets the allowed rollback actions whitelist. Each entry is a
// "module.action" string (e.g. "pkg.install", "file.copy"). A rollback step
// whose module.action is not listed is denied: it is not executed and the
// denial is recorded on the step result with a SkipReason that carries
// ErrStepNotWhitelisted so callers can detect denials programmatically.
//
// An EMPTY whitelist denies every rollback step (deny-by-default); use
// WithWhitelistAll for an explicit allow-all.
//
// The whitelist is the primary safety guard against a workflow author
// smuggling a destructive action into a RollbackSpec. Production deployments
// should always set an explicit whitelist instead of allowing everything.
func WithWhitelist(actions []string) ManagerOption {
	return func(m *Manager) {
		for _, a := range actions {
			m.whitelist[a] = true
		}
	}
}

// WithWhitelistAll explicitly allows every rollback step regardless of its
// module.action. It replaces the previous insecure default (an empty
// whitelist used to mean allow-all): opting into unrestricted rollback now
// requires this deliberate, greppable declaration. Tests and embedded use
// that genuinely want every step allowed should pass this option.
func WithWhitelistAll() ManagerOption {
	return func(m *Manager) { m.whitelistAll = true }
}

// WithConcurrency sets the per-batch target parallelism. n <= 0 is ignored
// (the default of 1 is kept). n == 1 means targets within a batch are rolled
// back serially; n > 1 means up to n targets are rolled back concurrently.
// Batches themselves are always rolled back serially in reverse order.
func WithConcurrency(n int) ManagerOption {
	return func(m *Manager) {
		if n > 0 {
			m.concurrency = n
		}
	}
}

// WithStopOnError controls whether the Manager stops at the first rollback
// step error. When true (the default), the Manager stops rolling back
// further batches as soon as a step fails; the result records the error and
// PartialRollback is set when earlier steps succeeded. When false, the
// Manager continues rolling back remaining batches and records all errors.
func WithStopOnError(b bool) ManagerOption {
	return func(m *Manager) {
		m.stopOnError = b
	}
}

// SnapshotRestoreFunc restores the pre-apply snapshot of a single plan step
// on a single target. It is supplied by the wiring layer (which owns the
// channel transport and the snapshot store); the rollback package stays
// transport-agnostic exactly like ExecuteFunc. runID is the closure run id
// under which the snapshot was captured.
type SnapshotRestoreFunc func(ctx context.Context, runID, target string, step plan.PlanStep) error

// WithSnapshotRestore installs the snapshot-restore callback used when a
// step's RollbackSpec.Strategy is "snapshot". Without it, a snapshot
// rollback step is recorded as skipped ("snapshot restore not wired") —
// never silently executed as an undo action. With it, the callback
// restores the captured files; a callback error marks the step failed.
func WithSnapshotRestore(fn SnapshotRestoreFunc) ManagerOption {
	return func(m *Manager) {
		m.snapshotRestore = fn
	}
}

// WithRunID records the closure run id the rollback belongs to. Snapshot
// lookup (strategy "snapshot") is keyed by this id; without it the
// restore callback receives an empty run id and fails closed.
func WithRunID(runID string) ManagerOption {
	return func(m *Manager) {
		m.runID = runID
	}
}

// SetRunID records the closure run id for the next Rollback invocation.
// The closure runner calls it right before triggering the rollback flow
// (the run id is minted per Run, after the Manager was constructed and
// injected). ClosureRunner is single-flight, so the write needs no lock.
func (m *Manager) SetRunID(runID string) {
	m.runID = runID
}

// NewManager returns a Manager configured by opts. The zero-value defaults
// are: empty whitelist (DENY every rollback step — use WithWhitelist to allow
// specific actions or WithWhitelistAll for an explicit allow-all),
// concurrency 1 (serial), stopOnError true.
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{
		whitelist:   make(map[string]bool),
		concurrency: 1,
		stopOnError: true,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// allows reports whether a "module.action" pair may be executed under the
// configured whitelist policy: WithWhitelistAll allows everything; otherwise
// the action must be listed explicitly (an empty whitelist denies all).
func (m *Manager) allows(moduleAction string) bool {
	if m.whitelistAll {
		return true
	}
	return m.whitelist[moduleAction]
}

// Whitelist returns the configured rollback whitelist in sorted order. The
// returned slice is a copy and may be safely modified by the caller. It is
// intended for diagnostics.
func (m *Manager) Whitelist() []string {
	out := make([]string, 0, len(m.whitelist))
	for k := range m.whitelist {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- Rollback --------------------------------------------------------------

// Rollback executes the rollback plan in reverse batch order WITHOUT execution
// evidence: every step of p is treated as having run (the pre-D-2 contract,
// kept for direct/manual callers that hold no apply result). Prefer
// RollbackWithLedger when the apply outcome is available.
func (m *Manager) Rollback(ctx context.Context, p *plan.Plan, execFn ExecuteFunc) *RollbackResult {
	return m.RollbackWithLedger(ctx, p, execFn, nil)
}

// RollbackWithLedger executes the rollback plan in reverse batch order,
// compensating exactly what the ledger says actually ran (D-2 v2).
//
// It walks p.Batches from last to first; within each batch it walks Steps from
// last to first; for each step with a non-nil RollbackSpec it executes the
// spec's Steps via execFn.
//
// Ledger semantics:
//
//   - ledger == nil: no evidence — every step is treated as run (legacy
//     behaviour for direct callers).
//   - ledger != nil: a step the ledger does not mark as run is skipped as
//     NotExecuted (benign: the forward action never happened, so there is
//     nothing to undo) and does not count against the verdict.
//   - a step the ledger marks as run but failing is still compensated, yet it
//     is flagged SideEffectsUnknown and counted in UnknownSideEffects —
//     informational per design (the extension point for a future
//     side-effect-unknown state); it does not flip the verdict on its own.
//
// Verdict (design D-2 v2 item 3):
//
//	RequiredCompensations = every executed (per the ledger) original step
//	                        — whether or not it declared compensation
//	CompletedCompensations = those whose compensation ran without error
//	Success = Error == nil && Required == Completed
//
// Compatibility (design item 2: nil = 旧行为): with a NIL ledger the
// pre-D-2 reading is preserved — skips (no spec, whitelist denial,
// snapshot not wired, nil execFn dry-run) are benign and only command
// errors fail the verdict. Evidence-backed gap counting engages as soon
// as a non-nil ledger is supplied.
//
// Behaviour:
//
//   - nil plan: returns a result with Error set (no panic).
//   - nil execFn: treated as "no-op" — all steps are recorded as skipped
//     with reason "no execute function". This is useful for dry-run
//     invocations that only want to see what would be rolled back.
//   - whitelist policy (deny-by-default): steps whose module.action is not
//     allowed (WithWhitelistAll or listed via WithWhitelist) are skipped
//     with reason "not in rollback whitelist".
//   - stopOnError true: the Manager stops at the first step error and
//     returns; remaining batches are not rolled back.
//   - stopOnError false: the Manager continues and records all errors.
//
// The returned *RollbackResult is always non-nil.
func (m *Manager) RollbackWithLedger(ctx context.Context, p *plan.Plan, execFn ExecuteFunc, ledger *ExecutionLedger) *RollbackResult {
	start := time.Now()
	result := &RollbackResult{}

	if p == nil {
		result.Error = fmt.Errorf("rollback: plan is nil")
		result.Duration = time.Since(start)
		return result
	}

	batchCount := len(p.Batches)
	result.BatchResults = make([]BatchRollbackResult, 0, batchCount)

	// Walk batches in reverse order. We track whether any executed step
	// succeeded and whether any failed to compute the partial-rollback
	// verdict at the end.
	var (
		firstErr      error
		stopRequested bool
	)

	for i := batchCount - 1; i >= 0; i-- {
		if stopRequested {
			break
		}
		batch := p.Batches[i]
		br := m.rollbackBatch(ctx, batch, execFn, ledger)

		// Compensation bookkeeping (D-2 v2): every step that RAN must be
		// accounted for — whether it declared a rollback, was refused by
		// policy, or failed. Skips for steps the ledger says never ran are
		// benign and excluded from the verdict.
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				if sr.SideEffectsUnknown {
					result.UnknownSideEffects++
				}
				// Benign skips, excluded from the verdict:
				//   - NotExecuted (evidence says the forward step never
				//     ran — nothing to account for);
				//   - any skip under a NIL ledger: without evidence every
				//     step is only ASSUMED run (design item 2: nil =
				//     旧行为), so policy/wiring/dry-run skips keep their
				//     pre-D-2 benign reading. Gap counting engages only
				//     once real evidence (a ledger) is supplied.
				if sr.NotExecuted || (ledger == nil && sr.Skipped) {
					continue
				}
				result.RequiredCompensations++
				switch {
				case sr.Skipped:
					// Evidence-backed gap: the step ran but no
					// compensation was applied (no rollback spec, not
					// whitelisted, snapshot restore wiring absent).
					// Counted as required-but-not-completed.
				case sr.Error != nil:
					if firstErr == nil {
						firstErr = sr.Error
					}
				default:
					result.CompletedCompensations++
				}
			}
		}
		result.BatchResults = append(result.BatchResults, br)

		if firstErr != nil && m.stopOnError {
			stopRequested = true
		}
	}

	result.Duration = time.Since(start)
	result.Error = firstErr
	// D-2 v2 verdict (design item 3): Success = no required compensation
	// missing AND no compensation command errored. The verdict reflects
	// RESTORED STATE, not merely "the flow ran to the end": a step that ran
	// forward but could not be compensated (no spec, whitelist denial,
	// snapshot wiring absent) keeps Success false. UnknownSideEffects stays
	// informational — the recorded extension point for the future
	// side-effect-unknown state.
	result.Success = result.Error == nil &&
		result.RequiredCompensations == result.CompletedCompensations
	result.PartialRollback = !result.Success && result.CompletedCompensations > 0
	return result
}

// rollbackBatch rolls back a single batch. Targets within the batch are
// rolled back concurrently up to m.concurrency. The returned BatchRollbackResult
// has TargetResults populated in completion order (non-deterministic under
// concurrency > 1). ledger carries the forward-execution evidence (nil = treat
// every step as run).
func (m *Manager) rollbackBatch(ctx context.Context, batch plan.Batch, execFn ExecuteFunc, ledger *ExecutionLedger) BatchRollbackResult {
	br := BatchRollbackResult{BatchIndex: batch.Index}

	targetCount := len(batch.Targets)
	if targetCount == 0 {
		return br
	}

	// Serial fast path: avoid goroutine overhead when concurrency is 1.
	if m.concurrency <= 1 {
		br.TargetResults = make([]TargetRollbackResult, 0, targetCount)
		for _, target := range batch.Targets {
			tr := m.rollbackTarget(ctx, target, batch.Steps, execFn, ledger)
			br.TargetResults = append(br.TargetResults, tr)
		}
		return br
	}

	// Concurrent path: fan out targets across workers, collecting results
	// in a guarded slice. A semaphore (buffered channel) caps the
	// parallelism at m.concurrency.
	br.TargetResults = make([]TargetRollbackResult, targetCount)
	sem := make(chan struct{}, m.concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for idx, target := range batch.Targets {
		wg.Add(1)
		go func(idx int, target string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			tr := m.rollbackTarget(ctx, target, batch.Steps, execFn, ledger)
			mu.Lock()
			br.TargetResults[idx] = tr
			mu.Unlock()
		}(idx, target)
	}
	wg.Wait()
	return br
}

// rollbackTarget rolls back all steps that ran on a single target. Steps are
// walked in reverse order (last executed first). For each step:
//
//   - no RollbackSpec: record skipped "no rollback spec".
//   - RollbackSpec present: execute each of its Steps (in declared order)
//     via execFn, after whitelist validation.
func (m *Manager) rollbackTarget(ctx context.Context, target string, steps []plan.PlanStep, execFn ExecuteFunc, ledger *ExecutionLedger) TargetRollbackResult {
	tr := TargetRollbackResult{
		Target:      target,
		StepResults: make([]StepRollbackResult, 0, len(steps)),
	}

	// Reverse step order: undo the last executed step first.
	for i := len(steps) - 1; i >= 0; i-- {
		ps := steps[i]

		// Evidence gate (D-2 v2): a step the ledger does not mark as run was
		// never dispatched to this target, so compensating it would touch a
		// host state that was never changed. Recorded as a benign skip.
		if ledger != nil && !ledger.Ran(target, ps.Name) {
			tr.StepResults = append(tr.StepResults, StepRollbackResult{
				OrigStepName: ps.Name,
				Skipped:      true,
				NotExecuted:  true,
				SkipReason:   "forward step not executed on this target",
			})
			continue
		}

		unknown := ledger != nil && ledger.SideEffectsUnknown(target, ps.Name)

		// No RollbackSpec: nothing to undo for this step. Because the step DID
		// run, this is a compensation gap (not a benign skip): the verdict must
		// not claim a clean rollback.
		if ps.Rollback == nil {
			tr.StepResults = append(tr.StepResults, StepRollbackResult{
				OrigStepName:       ps.Name,
				Skipped:            true,
				SkipReason:         "no rollback spec",
				SideEffectsUnknown: unknown,
			})
			continue
		}

		// Snapshot strategy: restore the pre-apply snapshot instead of
		// running undo steps (design 4.4.6.3 — the snapshot half of the
		// rollback protocol). The declared undo Steps are NOT executed:
		// a snapshot step declares its rollback basis as the captured
		// state, and mixing both would double-undo. Without a wired
		// restore callback the step is skipped with an explicit reason —
		// never silently executed as an undo action.
		if ps.Rollback.Strategy == "snapshot" {
			sr := m.restoreSnapshotStep(ctx, target, ps)
			sr.SideEffectsUnknown = unknown
			tr.StepResults = append(tr.StepResults, sr)
			continue
		}

		// RollbackSpec present: execute each declared rollback step. The
		// steps inside the spec run in declared order (the author chose
		// that order intentionally); only the outer plan-step order is
		// reversed.
		for _, rbStep := range ps.Rollback.Steps {
			sr := m.executeRollbackStep(ctx, target, ps.Name, rbStep, execFn)
			sr.SideEffectsUnknown = unknown
			tr.StepResults = append(tr.StepResults, sr)
		}
	}
	return tr
}

// restoreSnapshotStep rolls back one strategy-"snapshot" step on one
// target: it invokes the wired SnapshotRestoreFunc (which locates the
// pre-apply capture for m.runID/target/step and writes it back over the
// declared paths). Fail-closed paths:
//
//   - no restore callback wired: skipped, "snapshot restore not wired"
//     (the step never silently degrades into an undo action);
//   - no run id recorded: skipped, "snapshot restore without run id";
//   - callback error: the step is failed (not skipped) — a partial or
//     missing restore is operator-visible.
//
// The result carries the original step's module/action so audit output
// shows what was undone.
func (m *Manager) restoreSnapshotStep(ctx context.Context, target string, ps plan.PlanStep) StepRollbackResult {
	sr := StepRollbackResult{
		OrigStepName:     ps.Name,
		RollbackStepName: "snapshot:" + ps.Name,
		Module:           ps.Module,
		Action:           ps.Action,
	}
	if m.snapshotRestore == nil {
		sr.Skipped = true
		sr.SkipReason = "snapshot restore not wired"
		return sr
	}
	if m.runID == "" {
		sr.Skipped = true
		sr.SkipReason = "snapshot restore without run id"
		return sr
	}
	start := time.Now()
	err := m.snapshotRestore(ctx, m.runID, target, ps)
	sr.Duration = time.Since(start)
	if err != nil {
		sr.Error = fmt.Errorf("rollback snapshot of %s.%s on %s: %w", ps.Module, ps.Action, target, err)
	}
	return sr
}

// executeRollbackStep runs a single rollback step (from a RollbackSpec.Steps
// slice) on a single target, after whitelist validation.
func (m *Manager) executeRollbackStep(ctx context.Context, target, origStepName string, rbStep dsl.Step, execFn ExecuteFunc) StepRollbackResult {
	sr := StepRollbackResult{
		OrigStepName:     origStepName,
		RollbackStepName: rbStep.Name,
		Module:           rbStep.Module,
		Action:           rbStep.Action,
	}

	// Whitelist validation. Deny-by-default: a module.action is allowed only
	// when WithWhitelistAll was set or the pair is listed via WithWhitelist.
	// A denied step is skipped (never executed) and the skip reason embeds
	// ErrStepNotWhitelisted so callers can detect denials programmatically
	// (see NotWhitelisted).
	if !m.allows(rbStep.Module + "." + rbStep.Action) {
		sr.Skipped = true
		sr.SkipReason = fmt.Sprintf("%s.%s not in rollback whitelist: %v", rbStep.Module, rbStep.Action, ErrStepNotWhitelisted)
		return sr
	}

	// nil execFn: dry-run mode. Record as skipped so callers can see what
	// would have run without actually running it.
	if execFn == nil {
		sr.Skipped = true
		sr.SkipReason = "no execute function"
		return sr
	}

	start := time.Now()
	err := execFn(ctx, target, rbStep)
	sr.Duration = time.Since(start)
	if err != nil {
		sr.Error = fmt.Errorf("rollback %s.%s on %s: %w", rbStep.Module, rbStep.Action, target, err)
	}
	return sr
}

// --- helpers ---------------------------------------------------------------

// IsSkipped reports whether a StepRollbackResult was skipped (not executed).
// It is a convenience predicate for callers walking the result tree.
func IsSkipped(sr StepRollbackResult) bool { return sr.Skipped }

// HasError reports whether a StepRollbackResult represents a failed rollback
// step (executed and returned an error). Skipped steps are not errors.
func HasError(sr StepRollbackResult) bool { return !sr.Skipped && sr.Error != nil }

// ErrStepNotWhitelisted is the sentinel recorded (embedded in SkipReason) on
// rollback steps that were denied because their module.action was not allowed
// by the whitelist policy. Denied steps are never executed and are not errors;
// use NotWhitelisted to classify them programmatically.
var ErrStepNotWhitelisted = errors.New("rollback step not whitelisted")

// NotWhitelisted reports whether a StepRollbackResult was denied by the
// rollback whitelist policy. Denied steps are skipped (not executed) but are
// distinct from other skip reasons such as a missing RollbackSpec.
func NotWhitelisted(sr StepRollbackResult) bool {
	return sr.Skipped && strings.Contains(sr.SkipReason, ErrStepNotWhitelisted.Error())
}
