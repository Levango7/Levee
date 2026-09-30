package runstatus

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVocabularyHasNoDuplicates guards the package's own integrity: All must
// not repeat a status, and every derived set must be a subset of it. A
// duplicate would silently make one of the slice-based guards ambiguous.
func TestVocabularyHasNoDuplicates(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range All {
		require.False(t, seen[s], "duplicate status %q in All", s)
		seen[s] = true
		assert.True(t, IsValid(s))
	}
	for _, set := range []struct {
		name string
		vals []string
	}{
		{"Terminal", Terminal},
		{"RetryAdmitted", RetryAdmitted},
		{"RollbackAdmitted", RollbackAdmitted},
		{"RollbackVerdicts", RollbackVerdicts},
	} {
		local := map[string]bool{}
		for _, s := range set.vals {
			require.False(t, local[s], "duplicate %q in %s", s, set.name)
			local[s] = true
			assert.True(t, IsValid(s), "%s contains unknown status %q", set.name, s)
		}
	}
}

// TestRetiredVocabularyRejected pins the two values that caused real
// incidents, so they cannot be reintroduced as "just an alias".
//
//   - pending_approval: the web UI carried it as a second pending key, but the
//     state machine only ever produces "pending". REST list filters match
//     exactly, so the pending-approval tab and the mobile approve button
//     silently rendered nothing.
//   - count / by-tag / by-group: the parser accepted them while the plan
//     generator had no implementation, so such a workflow parsed cleanly and
//     then died with a Fatal LE034.
func TestRetiredVocabularyRejected(t *testing.T) {
	for _, retired := range []string{"pending_approval", "count", "by-tag", "by-group", "all", ""} {
		assert.False(t, IsValid(retired), "retired/garbage value %q must not validate", retired)
		assert.False(t, InRetryAdmitted(retired))
		assert.False(t, InRollbackAdmitted(retired))
	}
}

// TestCleanRollbackIsNotRollbackable states the double-undo guard: a fully
// restored change must not be rolled back again.
//
// Note the asymmetry with retry, which DOES admit rolled_back: re-running a
// successfully rolled-back change is a legitimate "try again with the fix",
// whereas re-rolling-back an already restored change has nothing left to undo.
// The compensation ledger makes that guard redundant rather than load-bearing.
func TestCleanRollbackIsNotRollbackable(t *testing.T) {
	assert.False(t, InRollbackAdmitted(StatusRolledBack),
		"rolling back an already restored change is a double undo")
	assert.True(t, InRetryAdmitted(StatusRolledBack),
		"retrying a cleanly rolled-back change is a legitimate re-drive")
	// The two partial verdicts ARE remedy entry points — that is their purpose.
	assert.True(t, InRetryAdmitted(StatusRolledBackPartial))
	assert.True(t, InRollbackAdmitted(StatusRolledBackPartial))
	assert.True(t, InRetryAdmitted(StatusRollbackIncomplete))
	assert.True(t, InRollbackAdmitted(StatusRollbackIncomplete))
	// A succeeded change may still be rolled back by an operator.
	assert.True(t, InRollbackAdmitted(StatusCompleted))
	// In-flight states are neither.
	assert.False(t, InRetryAdmitted(StatusRunning))
	assert.False(t, InRollbackAdmitted(StatusRunning))
	assert.False(t, InRetryAdmitted(StatusApproved))
}

// TestRollbackVerdictsSeparateCleanFromPartial encodes the rule the whole D-2
// line of work exists for: only rolled_back means "state restored".
func TestRollbackVerdictsSeparateCleanFromPartial(t *testing.T) {
	for _, s := range RollbackVerdicts {
		assert.True(t, IsRollbackVerdict(s))
		assert.True(t, IsTerminal(s), "%s is a terminal outcome", s)
	}
	assert.False(t, IsRollbackVerdict(StatusFailed))
	assert.False(t, IsRollbackVerdict(StatusCompleted))
	assert.False(t, IsRollbackVerdict(StatusInterrupted))
}

// TestTerminalDiffersFromSettlementGuard documents the two different meanings
// of "terminal" that once shared a name. grpc's terminalRunStatuses answers
// "may a late approval overwrite this row?" and therefore also contains
// approved and running; the lifecycle Terminal set must not.
func TestTerminalDiffersFromSettlementGuard(t *testing.T) {
	assert.False(t, IsTerminal(StatusApproved), "approved is not a lifecycle terminal")
	assert.False(t, IsTerminal(StatusRunning), "running is not a lifecycle terminal")
	assert.True(t, IsTerminal(StatusCompleted))
	assert.True(t, IsTerminal(StatusRolledBackPartial))
}

// TestJoinRendersOperatorText checks the derived error strings, since the whole
// point of deriving them is that they cannot contradict the guard.
func TestJoinRendersOperatorText(t *testing.T) {
	assert.Equal(t,
		"failed, rolled_back, rolled_back_partial, rollback_incomplete or interrupted",
		JoinRetryAdmitted())
	assert.Equal(t,
		"completed, failed, rolled_back_partial or rollback_incomplete",
		JoinRollbackAdmitted())
}
