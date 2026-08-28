package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPGStoreUpdateRunStatusIf_CAS verifies the compare-and-set status
// transition against a real PostgreSQL instance. Skipped when
// LEVEE_PG_TEST_DSN is not set.
func TestPGStoreUpdateRunStatusIf_CAS(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	run := &Run{
		ID:        "run-cas-pg-1",
		Status:    "approved",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
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

	// 2. Stale `from` fails and leaves the row untouched.
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
