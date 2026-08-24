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

	// Success reports whether every executed rollback step succeeded. A
	// rollback with only skipped steps (no RollbackSpec anywhere) is
	// considered successful: there was nothing to undo and no failure.
	Success bool

	// PartialRollback reports whether some rollback steps succeeded and
	// some failed. It is true only when the run was not fully successful
	// and at least one step completed without error. It lets the CLI
	// distinguish "rollback failed completely" from "rollback partially
	// applied" — the latter typically requires human inspection.
	PartialRollback bool

	// Error is the first error encountered during rollback, or nil when
	// Success is true. It is preserved separately from the per-step errors
	// so that callers can use errors.Is / errors.As on the top-level result
	// without walking the batch tree.
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
	// true, SkipReason explains why (no RollbackSpec, not in whitelist,
	// ...). Skipped steps are never errors.
	Skipped bool

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

// Rollback executes the rollback plan in reverse batch order. It walks
// p.Batches from last to first; within each batch it walks Steps from last to
// first; for each step with a non-nil RollbackSpec it executes the spec's
// Steps via execFn. Steps without a RollbackSpec are recorded as skipped.
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
// The returned *RollbackResult is always non-nil. Success is true when no
// executed step returned an error. PartialRollback is true when at least one
// step succeeded and at least one failed.
func (m *Manager) Rollback(ctx context.Context, p *plan.Plan, execFn ExecuteFunc) *RollbackResult {
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
		anySucceeded  bool
		anyFailed     bool
		stopRequested bool
	)

	for i := batchCount - 1; i >= 0; i-- {
		if stopRequested {
			break
		}
		batch := p.Batches[i]
		br := m.rollbackBatch(ctx, batch, execFn)

		// Inspect the batch result to update aggregate state.
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				if sr.Skipped {
					continue
				}
				if sr.Error != nil {
					anyFailed = true
					if firstErr == nil {
						firstErr = sr.Error
					}
				} else {
					anySucceeded = true
				}
			}
		}
		result.BatchResults = append(result.BatchResults, br)

		if anyFailed && m.stopOnError {
			stopRequested = true
		}
	}

	result.Duration = time.Since(start)
	result.Error = firstErr
	if anyFailed {
		result.Success = false
		result.PartialRollback = anySucceeded
	} else {
		// No failures: success regardless of how many steps were skipped.
		result.Success = true
		result.PartialRollback = false
	}
	return result
}

// rollbackBatch rolls back a single batch. Targets within the batch are
// rolled back concurrently up to m.concurrency. The returned BatchRollbackResult
// has TargetResults populated in completion order (non-deterministic under
// concurrency > 1).
func (m *Manager) rollbackBatch(ctx context.Context, batch plan.Batch, execFn ExecuteFunc) BatchRollbackResult {
	br := BatchRollbackResult{BatchIndex: batch.Index}

	targetCount := len(batch.Targets)
	if targetCount == 0 {
		return br
	}

	// Serial fast path: avoid goroutine overhead when concurrency is 1.
	if m.concurrency <= 1 {
		br.TargetResults = make([]TargetRollbackResult, 0, targetCount)
		for _, target := range batch.Targets {
			tr := m.rollbackTarget(ctx, target, batch.Steps, execFn)
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

			tr := m.rollbackTarget(ctx, target, batch.Steps, execFn)
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
func (m *Manager) rollbackTarget(ctx context.Context, target string, steps []plan.PlanStep, execFn ExecuteFunc) TargetRollbackResult {
	tr := TargetRollbackResult{
		Target:      target,
		StepResults: make([]StepRollbackResult, 0, len(steps)),
	}

	// Reverse step order: undo the last executed step first.
	for i := len(steps) - 1; i >= 0; i-- {
		ps := steps[i]

		// No RollbackSpec: nothing to undo for this step.
		if ps.Rollback == nil {
			tr.StepResults = append(tr.StepResults, StepRollbackResult{
				OrigStepName: ps.Name,
				Skipped:      true,
				SkipReason:   "no rollback spec",
			})
			continue
		}

		// RollbackSpec present: execute each declared rollback step. The
		// steps inside the spec run in declared order (the author chose
		// that order intentionally); only the outer plan-step order is
		// reversed.
		for _, rbStep := range ps.Rollback.Steps {
			sr := m.executeRollbackStep(ctx, target, ps.Name, rbStep, execFn)
			tr.StepResults = append(tr.StepResults, sr)
		}
	}
	return tr
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
