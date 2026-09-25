package rollback

import (
	"github.com/nexus/levee/internal/batch"
)

// ExecutionLedger records what actually ran during the forward apply, so a
// rollback can compensate exactly that and nothing else (D-2 v2).
//
// Why it exists: before this, rollback was handed the ORIGINAL batch structure
// (targets and steps as planned). A batch that failed mid-way therefore had its
// never-started steps "compensated" too, widening the blast radius of the undo
// beyond the work that had actually been applied.
//
// Two questions are tracked per (target, step):
//
//   - Ran: the step was dispatched to the target (its forward action was
//     attempted). Only such steps may be compensated.
//   - Unknown: the step was dispatched but FAILED, so whether it left a
//     partial side effect on the target cannot be determined from here. Such
//     steps are still given their declared compensation (the best available
//     remedy) and are counted in RollbackResult.UnknownSideEffects. Per the
//     D-2 v2 design the count is informational — the wired extension point
//     for a future side-effect-unknown state — and does not flip the
//     rollback verdict on its own.
//
// A third state records that an EARLIER rollback already compensated the
// step successfully:
//
//   - Compensated: the forward step ran AND its declared compensation
//     already completed without error. Re-running that compensation would
//     apply its side effects a second time (a non-idempotent undo — append a
//     line, bump a counter, open a ticket — lands twice), so such a step is
//     skipped with an explicit "already compensated" reason rather than
//     compensated again. MarkRan/MarkUnknown clear the flag, so a re-applied
//     step (retry) needs its compensation once more even if a previous
//     instance of it was already undone.
//
// A nil ledger means "no evidence available": every step of the passed plan is
// treated as having run (the pre-D-2 behaviour), which keeps direct/manual
// callers that have no apply result working.
type ExecutionLedger struct {
	ran         map[string]map[string]bool
	unknown     map[string]map[string]bool
	compensated map[string]map[string]bool
}

// NewExecutionLedger returns an empty ledger ready to be populated.
func NewExecutionLedger() *ExecutionLedger {
	return &ExecutionLedger{
		ran:         make(map[string]map[string]bool),
		unknown:     make(map[string]map[string]bool),
		compensated: make(map[string]map[string]bool),
	}
}

// MarkRan records that step ran on target (its forward action was attempted).
// It clears the compensated flag: a fresh execution of the same step needs its
// compensation again, even if an earlier execution of it was already undone.
func (l *ExecutionLedger) MarkRan(target, step string) {
	if l == nil || target == "" || step == "" {
		return
	}
	if l.ran == nil {
		l.ran = make(map[string]map[string]bool)
	}
	if l.ran[target] == nil {
		l.ran[target] = make(map[string]bool)
	}
	l.ran[target][step] = true
	if l.compensated[target] != nil {
		delete(l.compensated[target], step)
	}
}

// MarkCompensated records that step's declared compensation already completed
// successfully on target, so a later rollback must not run it again.
func (l *ExecutionLedger) MarkCompensated(target, step string) {
	if l == nil || target == "" || step == "" {
		return
	}
	if l.compensated == nil {
		l.compensated = make(map[string]map[string]bool)
	}
	if l.compensated[target] == nil {
		l.compensated[target] = make(map[string]bool)
	}
	l.compensated[target][step] = true
}

// AlreadyCompensated reports whether an earlier rollback already completed
// this step's compensation successfully on target.
func (l *ExecutionLedger) AlreadyCompensated(target, step string) bool {
	if l == nil || l.compensated == nil {
		return false
	}
	return l.compensated[target][step]
}

// MarkUnknown records that step ran on target but failed, leaving its side
// effects undetermined.
func (l *ExecutionLedger) MarkUnknown(target, step string) {
	if l == nil || target == "" || step == "" {
		return
	}
	l.MarkRan(target, step)
	if l.unknown == nil {
		l.unknown = make(map[string]map[string]bool)
	}
	if l.unknown[target] == nil {
		l.unknown[target] = make(map[string]bool)
	}
	l.unknown[target][step] = true
}

// Ran reports whether step was attempted on target.
func (l *ExecutionLedger) Ran(target, step string) bool {
	if l == nil || l.ran == nil {
		return false
	}
	return l.ran[target][step]
}

// SideEffectsUnknown reports whether step was attempted on target and failed.
func (l *ExecutionLedger) SideEffectsUnknown(target, step string) bool {
	if l == nil || l.unknown == nil {
		return false
	}
	return l.unknown[target][step]
}

// RanCount returns how many (target, step) pairs the ledger recorded as run.
func (l *ExecutionLedger) RanCount() int {
	if l == nil {
		return 0
	}
	n := 0
	for _, steps := range l.ran {
		n += len(steps)
	}
	return n
}

// CompensatedCount returns how many (target, step) pairs the ledger recorded as
// already compensated by an earlier rollback.
func (l *ExecutionLedger) CompensatedCount() int {
	if l == nil {
		return 0
	}
	n := 0
	for _, steps := range l.compensated {
		n += len(steps)
	}
	return n
}

// LedgerFromBatchResults derives the execution ledger from the closure's
// forward apply results (D-2 v2 design item 1: no new persistence — the
// evidence already exists). The batch controller appends a StepResult only
// for steps it actually dispatched through ExecuteFunc, so:
//
//   - a step with a nil error      → ran cleanly        → MarkRan
//   - a step with a non-nil error  → dispatched & failed → MarkUnknown
//     (MarkUnknown implies MarkRan: a failed forward step still needs its
//     declared compensation)
//   - a target/batch skipped before or between dispatches contributes no
//     step rows and therefore no ledger marks — compensating it would undo
//     state that was never changed.
//
// The returned ledger is never nil: an empty result set means "nothing ran",
// which is the correct reading of an apply that failed before its first
// dispatch.
func LedgerFromBatchResults(batchResults []*batch.BatchResult) *ExecutionLedger {
	l := NewExecutionLedger()
	for _, br := range batchResults {
		if br == nil {
			continue
		}
		for _, tr := range br.TargetResults {
			for _, sr := range tr.StepResults {
				if sr.Error != nil {
					l.MarkUnknown(tr.Target, sr.StepName)
				} else {
					l.MarkRan(tr.Target, sr.StepName)
				}
			}
		}
	}
	return l
}
