package rollback

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLedger_CompensatedStateMachine pins the third ledger state: a step that
// an earlier rollback already restored must not be compensated again — but a
// step that RAN AGAIN since then must be. Without the clearing behaviour in
// MarkRan, a retried apply (which re-executes the stored plan and rewrites the
// forward evidence) would leave the step marked compensated and its fresh
// side effects would never be undone.
func TestLedger_CompensatedStateMachine(t *testing.T) {
	l := NewExecutionLedger()

	// Not compensated until something says so.
	assert.False(t, l.AlreadyCompensated("h", "work"))

	l.MarkRan("h", "work")
	l.MarkCompensated("h", "work")
	assert.True(t, l.AlreadyCompensated("h", "work"))
	assert.Equal(t, 1, l.CompensatedCount())
	// Still a step that ran: the two states coexist, the gate reads them
	// independently.
	assert.True(t, l.Ran("h", "work"))

	// A re-apply clears it: the fresh execution needs compensating again.
	l.MarkRan("h", "work")
	assert.False(t, l.AlreadyCompensated("h", "work"),
		"MarkRan must clear compensated so a re-applied step is compensated again")
	assert.Equal(t, 0, l.CompensatedCount())

	// MarkUnknown (dispatched and failed) clears it too — its side effects are
	// undetermined, so its compensation must be attempted.
	l.MarkCompensated("h", "boom")
	l.MarkUnknown("h", "boom")
	assert.False(t, l.AlreadyCompensated("h", "boom"),
		"MarkUnknown must clear compensated")

	// Host isolation: compensating on one host says nothing about another.
	l.MarkCompensated("h1", "work")
	assert.True(t, l.AlreadyCompensated("h1", "work"))
	assert.False(t, l.AlreadyCompensated("h2", "work"))
}

func TestLedger_NilSafeForCompensated(t *testing.T) {
	var l *ExecutionLedger
	assert.NotPanics(t, func() {
		l.MarkCompensated("h", "work")
		l.MarkRan("h", "work")
	})
	assert.False(t, l.AlreadyCompensated("h", "work"))
	assert.Equal(t, 0, l.CompensatedCount())
}
