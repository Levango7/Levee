package approval

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// ExpirySweeper
// =========================================================================

func TestSweeper_SweepOnceSettlesAndReports(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	timedOut, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "high", ExpiresAt: pastTime()})
	require.NoError(t, err)
	fresh, err := svc.Create(ctx, CreateRequest{RunID: "run-2", Level: "standard", ExpiresAt: futureTime()})
	require.NoError(t, err)

	var reported []Approval
	sweeper := NewExpirySweeper(svc, time.Minute, func(_ context.Context, expired []Approval) {
		reported = append(reported, expired...)
	})

	settled, err := sweeper.SweepOnce(ctx)
	require.NoError(t, err)
	require.Len(t, settled, 1, "only the timed-out record settles")
	assert.Equal(t, timedOut.ID, settled[0].ID)
	assert.Equal(t, StatusExpired, settled[0].Status, "the record is post-state, not a preview")
	assert.Equal(t, "run-1", settled[0].RunID)
	assert.Equal(t, "high", settled[0].Level)

	// The callback saw exactly what SweepOnce returned.
	require.Len(t, reported, 1)
	assert.Equal(t, timedOut.ID, reported[0].ID)

	// The fresh record is untouched.
	got, err := svc.Get(ctx, fresh.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, got.Status)
}

func TestSweeper_SweepOnceNothingExpired(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "standard", ExpiresAt: futureTime()})
	require.NoError(t, err)
	_, err = svc.Create(ctx, CreateRequest{RunID: "run-2", Level: "standard"})
	require.NoError(t, err) // no timeout configured: CheckExpiry skips by contract

	var called int
	sweeper := NewExpirySweeper(svc, time.Minute, func(context.Context, []Approval) { called++ })

	settled, err := sweeper.SweepOnce(ctx)
	require.NoError(t, err)
	assert.Empty(t, settled)
	assert.Equal(t, 0, called, "an empty sweep must not fire the callback")
}

func TestSweeper_SweepOnceIdempotent(t *testing.T) {
	svc, _ := newService(t)
	ctx := bgCtx()

	_, err := svc.Create(ctx, CreateRequest{RunID: "run-1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)

	sweeper := NewExpirySweeper(svc, time.Minute, nil)

	first, err := sweeper.SweepOnce(ctx)
	require.NoError(t, err)
	require.Len(t, first, 1)

	// A second pass finds nothing: the record is already expired, and
	// re-expiring an expired record would corrupt the timeline.
	second, err := sweeper.SweepOnce(ctx)
	require.NoError(t, err)
	assert.Empty(t, second)
}

func TestSweeper_RunDisabledIntervalReturnsImmediately(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{RunID: "run-1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)

	// interval <= 0 is the documented opt-out: Run must return at once,
	// not tick, and not settle anything (the deployment said no).
	done := make(chan struct{})
	go func() {
		NewExpirySweeper(svc, 0, nil).Run(bgCtx())
		NewExpirySweeper(svc, -time.Minute, nil).Run(bgCtx())
		close(done)
	}()

	select {
	case <-done:
		// Returned without ticking.
	case <-time.After(2 * time.Second):
		t.Fatal("Run with a disabled interval must return immediately")
	}

	pending, err := svc.store.ListPending(bgCtx())
	require.NoError(t, err)
	assert.Len(t, pending, 1, "a disabled sweeper settles nothing")
}

func TestSweeper_RunStopsOnContextCancel(t *testing.T) {
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{RunID: "run-1", Level: "standard", ExpiresAt: pastTime()})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(bgCtx())
	sweeper := NewExpirySweeper(svc, 5*time.Millisecond, nil)
	done := make(chan struct{})
	go func() {
		sweeper.Run(ctx)
		close(done)
	}()

	// Give the first tick time to fire, then cancel: Run must exit.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run must exit when the context is cancelled")
	}
}

func TestSweeper_NilReceiverSafe(t *testing.T) {
	var sweeper *ExpirySweeper
	assert.NotPanics(t, func() { sweeper.Run(bgCtx()) },
		"serve wires the sweeper unconditionally; a nil one must be a no-op")
}
