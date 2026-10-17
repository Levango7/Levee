package state

// Batch and assignment state names.
//
// These two lists exist because the console mirrors both vocabularies in
// TypeScript, and a mirror maintained by hand is a second source: until now
// internal/state/{batch,assignment}_status_vocabulary_test.go parsed this file's
// constants AND the TypeScript copy on every run, so the guard could only notice
// that the copy disagreed with the parser, never that both disagreed with the
// server. The lists below are the data the generator (internal/docgen) writes
// into web/src/types/vocabulary.ts; the cross-language guard checks the wiring
// instead of re-parsing a transcription.
//
// The constants stay in store.go's const block on purpose: they are used in
// switch cases, and a switch needs constants, so "declare the set once as data"
// cannot mean "delete the constants". What it does mean is that the order-bearing
// list has exactly one home, and that a constant added without joining it turns a
// test red here (vocabulary_ownership_test.go) rather than silently widening the
// wire vocabulary nobody documented.
//
// Do NOT merge the lists. `pending` / `done` / `interrupted` are spelled the
// same on both sides but mean different things — a batch's `pending` is "not
// started", an assignment's `pending` is "not yet claimed by a worker" — and the
// result group (AssignResult*) is a separate value range, named below as its own
// list rather than folded into either.
//
// The step / run-approval / result lists were added for the same reason the two
// above were: the console renders steps.status (web/src/utils/targetMap.ts held a
// hand copy of it) and prose elsewhere names these sets, and a hand copy checked
// by re-parsing the copy cannot see the copy and the owner agreeing on a wrong
// answer. Each one is still ONE list with ONE meaning; where two columns share
// spellings they get two lists and two names, never one merged list.

// batchStateOrder lists the spellings a batch row can carry. The order is the
// monitor page's rendering order: lifecycle first, then the legacy `done`, then
// the failure verdicts. `completed` is what the writer emits (see
// internal/wiring/persist.go); `done` is the older spelling the schema comment
// still documents and nothing writes today, and it stays named because a future
// writer must not invent a third word for "finished".
var batchStateOrder = []string{
	BatchStatePending,
	BatchStateRunning,
	BatchStateCompleted,
	BatchStateDone,
	BatchStateFailed,
	BatchStateRolledBack,
	BatchStateInterrupted,
}

// assignmentStateOrder lists the values of assignments.state in dispatch order.
var assignmentStateOrder = []string{
	AssignStatePending,
	AssignmentStateExecuting,
	AssignmentStateDone,
	AssignmentStateInterrupted,
}

// BatchStateNames returns the batch vocabulary in rendering order. The result is
// a copy and may be modified by the caller.
func BatchStateNames() []string {
	return append([]string(nil), batchStateOrder...)
}

// AssignmentStateNames returns the assignment vocabulary in dispatch order. The
// result is a copy and may be modified by the caller.
func AssignmentStateNames() []string {
	return append([]string(nil), assignmentStateOrder...)
}

// stepStatusOrder lists the spellings a steps.status row can carry, in the order
// the monitor page renders them. Pending and running have no writer today (see the
// note on the constants in store.go): they stay listed because the schema comment
// names them and internal/lock's busy-host probe reads them, and a future writer
// must not invent a third spelling for a state that is already named here.
var stepStatusOrder = []string{
	StepStatusPending,
	StepStatusRunning,
	StepStatusSuccess,
	StepStatusFailed,
	StepStatusSkipped,
}

// runApprovalStatusOrder lists the verdict a RUN row carries
// (runs.approval_status). It is NOT the approvals table's status column: that one
// has a fourth member (expired) and lives in internal/approval, which names its
// own list. schema.sql's comment on this column advertises `timeout` and `skipped`,
// and both belong over there — the list below was swept from the writers, not
// copied from that comment, and the divergence is recorded rather than smoothed
// over (see TestRunApprovalStatusListCoversEveryConstant).
var runApprovalStatusOrder = []string{
	ApprovalStatusPending,
	ApprovalStatusApproved,
	ApprovalStatusRejected,
}

// assignResultOrder lists the results assignments.result names
// (assignments.result). The column's SQL default is ” — unset — so the empty
// string is a value the wire can carry that is deliberately NOT in this list:
// it means "no result recorded yet", not a fourth verdict, and a rendering that
// folds it into `completed` would claim work that never finished. Nothing in the
// console reads this column today (AssignmentSummary groups by state), so the list
// exists to give the vocabulary one home before a reader appears.
var assignResultOrder = []string{
	AssignResultCompleted,
	AssignResultFailed,
	AssignResultRolledBack,
}

// StepStatusNames returns the step vocabulary in rendering order. The result is a
// copy and may be modified by the caller.
func StepStatusNames() []string {
	return append([]string(nil), stepStatusOrder...)
}

// ApprovalStatusNames returns the run-side approval vocabulary in decision order.
// The result is a copy and may be modified by the caller.
func ApprovalStatusNames() []string {
	return append([]string(nil), runApprovalStatusOrder...)
}

// AssignResultNames returns the assignment-result vocabulary. The result is a copy
// and may be modified by the caller.
func AssignResultNames() []string {
	return append([]string(nil), assignResultOrder...)
}
