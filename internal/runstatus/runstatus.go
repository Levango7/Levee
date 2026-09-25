// Package runstatus is the single source of truth for the LEVEE run / change
// status vocabulary.
//
// Before this package the vocabulary existed only as string literals spread
// across the state machine, the gRPC layer, the CLI terminal-state matrices,
// metrics labels, proto comments and a hand-maintained mirror in the web UI.
// Every one of those was a potential copy to drift — and two of them already
// did: the UI carried a `pending_approval` key the backend never produced (so
// the pending-approval list and the mobile approve button silently rendered
// nothing), and the proto comments still listed the retired value while
// omitting the D-2 v2 rollback verdicts.
//
// The rule now: no package may spell a run status as a bare string literal.
// Reference these constants, and add a vocabulary test whenever a status is
// introduced. The web UI cannot import Go, so web/src keeps a mirror — but
// TestWebStatusMirrorMatches pins the two key sets together (run
// `go test ./tests/...` or the CI `test` job; it is the same gate that already
// guards the rollback verdicts' labels and colours).
package runstatus

import "strings"

// Run status values. A run row records the lifecycle position of one Change.
const (
	// StatusDraft — created by CreateChange / CloneChange; a plan exists or
	// is about to be generated, nothing has been approved.
	StatusDraft = "draft"
	// StatusPending — created by InstantiateTemplate (normal path); waiting
	// for approval to be settled.
	StatusPending = "pending"
	// StatusPlanned — created by InstantiateTemplate (dry-run preview); a plan
	// was generated but nothing was dispatched.
	StatusPlanned = "planned"
	// StatusApproved — the plan version bound to the approval is settled and
	// ready to apply.
	StatusApproved = "approved"
	// StatusRunning — dispatched and in flight.
	StatusRunning = "running"
	// StatusPaused — suspended by an operator; resumable.
	StatusPaused = "paused"
	// StatusCompleted — every batch finished successfully.
	StatusCompleted = "completed"
	// StatusFailed — execution failed (rollback outcome is carried by the
	// rollback verdicts below, not by this status).
	StatusFailed = "failed"
	// StatusCancelled — abandoned by an operator.
	StatusCancelled = "cancelled"
	// StatusRolledBack — a clean rollback: every required compensation
	// completed. Never use it when any compensation is missing.
	StatusRolledBack = "rolled_back"
	// StatusRolledBackPartial — D-2 v2 verdict: the rollback completed some
	// but not all required compensations, so the state is only partly
	// restored. Distinct from StatusRolledBack on purpose.
	StatusRolledBackPartial = "rolled_back_partial"
	// StatusRollbackIncomplete — D-2 v2 verdict: no required compensation
	// completed. Also the entry state for a retry-driven remedy.
	StatusRollbackIncomplete = "rollback_incomplete"
	// StatusRejected — an approver rejected the change.
	StatusRejected = "rejected"
	// StatusArchived — sealed history. Every terminal outcome is archivable,
	// including the two rollback verdicts.
	StatusArchived = "archived"
	// StatusInterrupted — terminal: the executor node died mid-flight and the
	// cluster takeover settled the run (cluster mode only). Re-drive
	// explicitly via RetryChange.
	StatusInterrupted = "interrupted"
)

// All lists every run status, in roughly lifecycle order.
var All = []string{
	StatusDraft,
	StatusPending,
	StatusPlanned,
	StatusApproved,
	StatusRunning,
	StatusPaused,
	StatusCompleted,
	StatusFailed,
	StatusCancelled,
	StatusRolledBack,
	StatusRolledBackPartial,
	StatusRollbackIncomplete,
	StatusRejected,
	StatusArchived,
	StatusInterrupted,
}

// Terminal lists the statuses from which the run has reached an outcome and
// will make no further automatic progress.
//
// NOTE: this is NOT the same set as grpc's terminalRunStatuses, which answers
// a different question — "may a late-settling approval overwrite this row?"
// and therefore also includes StatusApproved and StatusRunning (an approval
// that settles after the run moved on must not resurrect or demote it). The
// two sets were both called "terminal", which is exactly the kind of
// vocabulary collision this package exists to remove. Keep them distinct.
var Terminal = []string{
	StatusCompleted,
	StatusFailed,
	StatusCancelled,
	StatusRolledBack,
	StatusRolledBackPartial,
	StatusRollbackIncomplete,
	StatusRejected,
	StatusArchived,
	StatusInterrupted,
}

// RetryAdmitted lists the statuses RetryChange accepts. Every one of them is a
// failure-family outcome, which is what makes retry a remedy entry point
// rather than a way to re-run something still in flight.
//
// Mirrors the guard in ChangeService.RetryChange; the test
// TestRetryAdmittedMatchesServiceGuard in internal/grpc keeps them in step.
var RetryAdmitted = []string{
	StatusFailed,
	StatusRolledBack,
	StatusRolledBackPartial,
	StatusRollbackIncomplete,
	StatusInterrupted,
}

// RollbackAdmitted lists the statuses RollbackChange accepts.
//
// Mirrors the guard in ChangeService.RollbackChange; TestRollbackAdmitted-
// MatchesServiceGuard in internal/grpc keeps them in step. Note the shape:
// StatusCompleted IS admitted (an operator may roll back a change that
// succeeded), while a clean StatusRolledBack is NOT — re-rolling back an
// already restored change is a double undo, and the D-2 compensation ledger
// is there to make that guard redundant rather than load-bearing.
var RollbackAdmitted = []string{
	StatusCompleted,
	StatusFailed,
	StatusRolledBackPartial,
	StatusRollbackIncomplete,
}

// JoinRetryAdmitted renders the RetryChange admission set for operator-facing
// error text, so the message can never contradict the guard.
func JoinRetryAdmitted() string { return humanList(RetryAdmitted) }

// JoinRollbackAdmitted renders the RollbackChange admission set likewise.
func JoinRollbackAdmitted() string { return humanList(RollbackAdmitted) }

// humanList renders a vocabulary slice as "a, b, c or d".
func humanList(set []string) string {
	switch len(set) {
	case 0:
		return ""
	case 1:
		return set[0]
	}
	return strings.Join(set[:len(set)-1], ", ") + " or " + set[len(set)-1]
}

// inSet reports membership in one of the vocabulary slices above.
func inSet(set []string, s string) bool {
	for _, known := range set {
		if s == known {
			return true
		}
	}
	return false
}

// IsValid reports whether s is a member of the vocabulary. Used by the gRPC
// layer to reject unknown filter values instead of silently matching nothing —
// the failure mode that made the UI's stale `pending_approval` key invisible:
// the REST list filter matches exactly, so a status nothing produces yields an
// empty table and no error anywhere.
func IsValid(s string) bool { return inSet(All, s) }

// IsTerminal reports whether s has reached an outcome and will make no further
// automatic progress. See Terminal for why this is NOT grpc's
// terminalRunStatuses.
func IsTerminal(s string) bool { return inSet(Terminal, s) }

// InRetryAdmitted reports whether RetryChange accepts a run in status s.
func InRetryAdmitted(s string) bool { return inSet(RetryAdmitted, s) }

// InRollbackAdmitted reports whether RollbackChange accepts a run in status s.
func InRollbackAdmitted(s string) bool { return inSet(RollbackAdmitted, s) }

// IsRollbackVerdict reports whether s is one of the three rollback outcomes.
// Only StatusRolledBack among them means "state fully restored"; the other two
// must never be presented as a clean rollback, in the API or the UI.
func IsRollbackVerdict(s string) bool { return inSet(RollbackVerdicts, s) }

// RollbackVerdicts lists the three terminal rollback outcomes.
var RollbackVerdicts = []string{
	StatusRolledBack,
	StatusRolledBackPartial,
	StatusRollbackIncomplete,
}
