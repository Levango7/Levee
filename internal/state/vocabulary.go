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
// Do NOT merge the two lists. `pending` / `done` / `interrupted` are spelled the
// same on both sides but mean different things — a batch's `pending` is "not
// started", an assignment's `pending` is "not yet claimed by a worker" — and the
// result group (AssignResult*) is a third value range that this file deliberately
// does not name.

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
