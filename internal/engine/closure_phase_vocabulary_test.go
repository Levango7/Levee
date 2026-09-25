package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nexus/levee/internal/runstatus"
)

// TestClosurePhasesMapToRunStatuses pins the correspondence between the
// closure execution phases and the run status vocabulary.
//
// ClosureResult.Phase is what wiring turns into run.status (see
// internal/wiring/run.go settleRun), so the two vocabularies are not
// independent: a phase whose string drifts from its run status would produce a
// status the UI has no label for, the REST filter cannot match, and the
// RetryChange / RollbackChange admission guards reject — silently, in the
// sense that nothing crashes, the change just becomes unrecoverable by hand.
//
// This is why the closure phases are allowed to keep their own constants
// rather than reusing runstatus: they are a distinct concept (what the
// closure did) that happens to share string values. This test is the contract.
func TestClosurePhasesMapToRunStatuses(t *testing.T) {
	cases := []struct {
		phase     ClosurePhase
		runStatus string
	}{
		{PhaseRolledBack, runstatus.StatusRolledBack},
		{PhasePartialRollback, runstatus.StatusRolledBackPartial},
		{PhaseRollbackIncomplete, runstatus.StatusRollbackIncomplete},
		{PhaseCompleted, runstatus.StatusCompleted},
		{PhaseFailed, runstatus.StatusFailed},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.runStatus, string(tc.phase),
			"closure phase %q must carry the same string as its run status", tc.phase)
		assert.True(t, runstatus.IsValid(string(tc.phase)),
			"closure phase %q must be a valid run status", tc.phase)
	}
}

// TestPartialRollbackPhasesAreRecoverable restates, from the engine side, that
// the two incomplete verdicts are remedy entry points rather than dead ends.
func TestPartialRollbackPhasesAreRecoverable(t *testing.T) {
	for _, phase := range []ClosurePhase{PhasePartialRollback, PhaseRollbackIncomplete} {
		s := string(phase)
		assert.True(t, runstatus.InRetryAdmitted(s), "%s must be retryable", s)
		assert.True(t, runstatus.InRollbackAdmitted(s), "%s must be re-drivable by rollback", s)
		assert.False(t, runstatus.IsTerminal(runstatus.StatusRolledBack) && s == string(PhaseRolledBack),
			"sanity: a clean rollback is terminal but not re-drivable")
	}
	// A clean rollback is terminal, and must NOT accept another rollback.
	assert.True(t, runstatus.IsTerminal(runstatus.StatusRolledBack))
	assert.False(t, runstatus.InRollbackAdmitted(string(PhaseRolledBack)))
}
