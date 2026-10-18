package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// newRunEventStoreFixture opens an in-memory sqlite store wrapped in the
// run-event decorator, and seeds one run in the given status.
func newRunEventStoreFixture(t *testing.T, initialStatus string) (*RunEventStore, state.Store, string) {
	t.Helper()
	ctx := context.Background()
	base, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })

	run := &state.Run{
		ID:        "chg-fixture",
		Status:    initialStatus,
		Creator:   "alice",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, base.CreateRun(ctx, run))

	decorated := NewRunEventStore(base)
	return decorated, base, run.ID
}

// RunEventRecord is what the tests capture from the sink. A distinct type
// (rather than positional captures) keeps each assertion readable.
type RunEventRecord struct {
	ChangeID, From, To string
	At                 time.Time
}

func captureSink(s *RunEventStore) func() []RunEventRecord {
	var recs []RunEventRecord
	s.SetSink(func(changeID, from, to string, at time.Time) {
		recs = append(recs, RunEventRecord{ChangeID: changeID, From: from, To: to, At: at})
	})
	return func() []RunEventRecord { return recs }
}

func TestRunEventStore_UpdateRunStatusIfEmitsOnAppliedTransition(t *testing.T) {
	s, _, id := newRunEventStoreFixture(t, "approved")
	events := captureSink(s)

	ok, err := s.UpdateRunStatusIf(context.Background(), id, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok, "fixture must start in the from-status")

	recs := events()
	require.Len(t, recs, 1)
	assert.Equal(t, id, recs[0].ChangeID)
	assert.Equal(t, "approved", recs[0].From)
	assert.Equal(t, "running", recs[0].To)
}

func TestRunEventStore_RefusedCASEmitsNothing(t *testing.T) {
	s, _, id := newRunEventStoreFixture(t, "approved")
	events := captureSink(s)

	// The row is "approved": a CAS from "running" does not apply, and a
	// transition that did not happen must not be reported.
	ok, err := s.UpdateRunStatusIf(context.Background(), id, "running", "failed", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Empty(t, events())
}

func TestRunEventStore_ApprovalSettlementCASEmits(t *testing.T) {
	s, _, id := newRunEventStoreFixture(t, "pending_approval")
	events := captureSink(s)

	ok, err := s.UpdateRunApprovalStatusIf(context.Background(), id, "pending_approval", "failed", "rejected", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	recs := events()
	require.Len(t, recs, 1, "the approval settlement's run-status half is a transition")
	assert.Equal(t, "pending_approval", recs[0].From)
	assert.Equal(t, "failed", recs[0].To)
}

func TestRunEventStore_UpdateRunEmitsOnlyRealStatusChange(t *testing.T) {
	s, base, id := newRunEventStoreFixture(t, "running")
	events := captureSink(s)
	ctx := context.Background()

	// Metadata-only full-row write (replan persists the plan hash): the
	// status is unchanged, nothing to report.
	run, err := base.GetRun(ctx, id)
	require.NoError(t, err)
	run.PlanHash = "sha:replanned"
	require.NoError(t, s.UpdateRun(ctx, run))
	assert.Empty(t, events(), "a metadata write must not emit")

	// Status-carrying full-row write (pause path): emits from → to.
	run.Status = "paused"
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, s.UpdateRun(ctx, run))

	recs := events()
	require.Len(t, recs, 1)
	assert.Equal(t, "running", recs[0].From)
	assert.Equal(t, "paused", recs[0].To)
}

func TestRunEventStore_NilSinkDropsEvents(t *testing.T) {
	s, _, id := newRunEventStoreFixture(t, "approved")

	// No SetSink call: the decorator is inert. A deployment (and every
	// test fixture) without a consumer gets the plain store behaviour.
	ok, err := s.UpdateRunStatusIf(context.Background(), id, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, ok, "the write must succeed with no sink installed")
}

func TestRunEventStore_SinkPanicContained(t *testing.T) {
	s, _, id := newRunEventStoreFixture(t, "approved")
	s.SetSink(func(changeID, from, to string, at time.Time) {
		panic("a misbehaving consumer must not fail the store write")
	})

	// The write goes through; the panic is contained and logged.
	ok, err := s.UpdateRunStatusIf(context.Background(), id, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestRunEventStore_UnknownRunIsTransparent(t *testing.T) {
	s, base, _ := newRunEventStoreFixture(t, "approved")
	events := captureSink(s)
	ctx := context.Background()

	// UpdateRun on an unknown id: the base rejects it ("not found"), and
	// transparent means forwarding THAT — the decorator neither invents a
	// success nor masks the failure. Nothing is emitted either way: a
	// write that did not happen is not a transition.
	run := &state.Run{ID: "chg-missing", Status: "running", UpdatedAt: time.Now().UTC()}
	err := s.UpdateRun(ctx, run)
	require.Error(t, err, "the base's not-found error must come through untouched")
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, events())
	_, _ = base.GetRun(ctx, "chg-missing")
}

// TestRunEventStore_UnwrapperSeam pins the Unwrapper participation: the
// state.Underlying walk must reach the raw backend THROUGH this decorator.
// The agent registry builds on that walk; a decorator the walk cannot see
// through made it serve an Unimplemented stub (caught by
// TestServePolicyRefusesEveryUngrantedCall's nil deref on macOS).
func TestRunEventStore_UnwrapperSeam(t *testing.T) {
	s, base, _ := newRunEventStoreFixture(t, "running")
	assert.Equal(t, base, s.Underlying(), "Underlying() must hand back the wrapped store")
	assert.Equal(t, base, state.Underlying(s), "the state walk must see through the decorator")
}
