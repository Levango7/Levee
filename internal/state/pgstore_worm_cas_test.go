// pgstore_worm_cas_test.go covers the PostgreSQL-specific behaviour of the
// newer Store methods: WORM-protected cascade deletes and the compare-and-set
// helpers. All tests are env-gated on LEVEE_PG_TEST_DSN (see pgstore_test.go)
// and are skipped when no PostgreSQL instance is available.
package state

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPG_DeleteRun_WORMTracesBlockDelete(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "pg-run-worm", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "completed", CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.CreateTrace(ctx, &Trace{
		ID: "pg-tr-worm", RunID: "pg-run-worm", Event: "plan", Actor: "u",
		Detail: `{}`, CurrHash: "hash", Timestamp: now,
	}))

	// The FK cascade fires worm_prevent_trace_delete, so the whole delete
	// is rejected instead of silently removing audit history.
	err := store.DeleteRun(ctx, "pg-run-worm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM")

	traces, err := store.ListTraces(ctx, TraceFilter{RunID: "pg-run-worm"})
	require.NoError(t, err)
	assert.Len(t, traces, 1)

	// A run without traces deletes normally.
	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "pg-run-clean", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "completed", CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.DeleteRun(ctx, "pg-run-clean"))
}

func TestPG_UpdateApprovalIfPending_CAS(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "pg-run-appr", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "pending",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	timeout := now.Add(time.Hour)
	require.NoError(t, store.CreateApproval(ctx, &Approval{
		ID: "pg-appr-1", RunID: "pg-run-appr", Level: "high",
		Status: "pending", Comment: "{}", TimeoutAt: &timeout,
	}))

	loaded, err := store.GetApproval(ctx, "pg-appr-1")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	loaded.Status = "approved"
	loaded.Approver = "alice"
	ok, err := store.UpdateApprovalIfPending(ctx, loaded)
	require.NoError(t, err)
	assert.True(t, ok)

	stale := &Approval{ID: "pg-appr-1", Status: "rejected"}
	ok, err = store.UpdateApprovalIfPending(ctx, stale)
	require.NoError(t, err)
	assert.False(t, ok, "terminal rows must refuse the CAS")

	got, err := store.GetApproval(ctx, "pg-appr-1")
	require.NoError(t, err)
	assert.Equal(t, "approved", got.Status)
}

func TestPG_UpdateTraceChecksum(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: "pg-run-chk", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
	require.NoError(t, store.CreateTrace(ctx, &Trace{
		ID: "pg-tr-1", RunID: "pg-run-chk", Event: "plan", Actor: "u",
		Detail: `{}`, Timestamp: now,
	}))

	checksum := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, store.UpdateTraceChecksum(ctx, "pg-tr-1", checksum))

	got, err := store.GetTrace(ctx, "pg-tr-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, checksum, got.CurrHash)

	// Only empty curr_hash may be filled; existing values are immutable.
	err = store.UpdateTraceChecksum(ctx, "pg-tr-1", "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	require.Error(t, err)
}

func TestPG_DeleteLockByIDAndOwner(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, store.CreateLock(ctx, &Lock{
		ID: "pg-lock-1", Scope: "host:h1", Owner: "run-a", TTLSeconds: 60,
		AcquiredAt: now, ExpiresAt: now.Add(time.Minute),
	}))

	deleted, err := store.DeleteLockByIDAndOwner(ctx, "pg-lock-1", "run-b")
	require.NoError(t, err)
	assert.False(t, deleted, "wrong owner must not delete")

	deleted, err = store.DeleteLockByIDAndOwner(ctx, "pg-lock-1", "run-a")
	require.NoError(t, err)
	assert.True(t, deleted)
}
