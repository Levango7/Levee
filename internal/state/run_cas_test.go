package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateRunStatusIf_CAS verifies the compare-and-set status transition
// used to guard ApplyChange against concurrent racers: only the expected
// `from` state can transition, a concurrent change is reported via the
// boolean result (not an error), and a missing run is reported as false.
func TestUpdateRunStatusIf_CAS(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	run := &Run{
		ID:        "run-cas-1",
		Status:    "approved",
		UpdatedAt: time.Now().UTC(),
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, store.CreateRun(ctx, run))

	// 1. Expected transition succeeds and persists.
	now := time.Now().UTC()
	ok, err := store.UpdateRunStatusIf(ctx, run.ID, "approved", "running", now)
	require.NoError(t, err)
	assert.True(t, ok, "approved -> running must be applied")

	got, err := store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status)
	assert.Equal(t, now.UTC().Truncate(time.Second), got.UpdatedAt.UTC().Truncate(time.Second))

	// 2. Re-applying the same transition now fails (status is no longer
	//    approved) and leaves the row untouched.
	ok, err = store.UpdateRunStatusIf(ctx, run.ID, "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, ok, "stale `from` must not transition")

	got, err = store.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", got.Status, "failed CAS must not mutate status")

	// 3. Missing run reports false, not an error.
	ok, err = store.UpdateRunStatusIf(ctx, "run-does-not-exist", "approved", "running", time.Now().UTC())
	require.NoError(t, err)
	assert.False(t, ok, "missing run must report false")
}
