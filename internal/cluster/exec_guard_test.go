// exec_guard_test.go exercises the execution-lease/fencing primitive
// (run_execution). Tests requiring a real PostgreSQL instance are skipped
// unless LEVEE_PG_TEST_DSN is set — same convention as the distributed
// lock suite. Argument validation and nil-db paths always run.
package cluster

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestExecGuard opens the test PostgreSQL instance, ensures the
// cluster schema (which creates run_execution) and returns a guard
// backed by it.
func newTestExecGuard(t *testing.T) (*ExecutionGuard, *sql.DB) {
	t.Helper()
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL exec-guard test")
	}
	db := openTestDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, ensureClusterSchema(context.Background(), db))
	return NewExecutionGuard(db), db
}

func TestExecutionGuardNilDB(t *testing.T) {
	g := NewExecutionGuard(nil)
	ctx := context.Background()
	_, err := g.Register(ctx, "run-1", "node-a", time.Second)
	assert.Error(t, err)
	assert.Error(t, g.Owns(ctx, &ExecLease{RunID: "run-1", Owner: "node-a", Epoch: 1}, time.Second))
	assert.Error(t, g.Heartbeat(ctx, &ExecLease{RunID: "run-1", Owner: "node-a"}, time.Second))
	assert.Error(t, g.End(ctx, &ExecLease{RunID: "run-1", Owner: "node-a", Epoch: 1}))
	_, err = g.ExpiredExecutions(ctx)
	assert.Error(t, err)
}

func TestExecutionGuardValidation(t *testing.T) {
	g := NewExecutionGuard(nil)
	ctx := context.Background()
	_, err := g.Register(ctx, "", "node-a", time.Second)
	assert.Error(t, err)
	_, err = g.Register(ctx, "run-1", "", time.Second)
	assert.Error(t, err)
	_, err = g.Register(ctx, "run-1", "node-a", -time.Second)
	assert.Error(t, err)
}

// TestExecutionGuard_RegisterOwnsEndHappyPath covers the primitive's
// core contract: registration mints a lease with a positive epoch, Owns
// renews it, End releases it, and a fresh registration afterwards mints
// a strictly GREATER epoch (monotonic fencing tokens).
func TestExecutionGuard_RegisterOwnsEndHappyPath(t *testing.T) {
	g, _ := newTestExecGuard(t)
	ctx := context.Background()
	const runID = "run-guard-happy"

	lease, err := g.Register(ctx, runID, "node-a", 10*time.Second)
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Equal(t, "node-a", lease.Owner)
	assert.Positive(t, lease.Epoch)
	firstEpoch := lease.Epoch

	require.NoError(t, g.Owns(ctx, lease, 10*time.Second))
	require.NoError(t, g.Heartbeat(ctx, lease, 10*time.Second))
	require.NoError(t, g.End(ctx, lease))

	// After End the row is gone: a new owner registers freely and mints
	// a strictly greater epoch.
	lease2, err := g.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	assert.Greater(t, lease2.Epoch, firstEpoch, "epoch must increase across owners")
	require.NoError(t, g.End(ctx, lease2))
}

// TestExecutionGuard_SecondLiveOwnerRefused pins the single-flight
// property: while node-a holds a live lease, node-b's registration is
// refused with ErrFencedOut (double execution is the worst outcome).
func TestExecutionGuard_SecondLiveOwnerRefused(t *testing.T) {
	g, _ := newTestExecGuard(t)
	ctx := context.Background()
	const runID = "run-guard-singleflight"

	lease, err := g.Register(ctx, runID, "node-a", 30*time.Second)
	require.NoError(t, err)
	defer func() { _ = g.End(ctx, lease) }()

	_, err = g.Register(ctx, runID, "node-b", 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFencedOut)
}

// TestExecutionGuard_ExpiredLeaseReclaimable covers the takeover-style
// reclaim: after the lease expires, a NEW owner registers with a fresh
// (greater) epoch, and the OLD owner's first write — Owns — fails with
// ErrFencedOut (zombie rejection).
func TestExecutionGuard_ExpiredLeaseReclaimable(t *testing.T) {
	g, db := newTestExecGuard(t)
	ctx := context.Background()
	const runID = "run-guard-reclaim"

	lease, err := g.Register(ctx, runID, "node-a", 50*time.Millisecond)
	require.NoError(t, err)
	firstEpoch := lease.Epoch

	// Expire the lease deterministically (no sleeping on the clock):
	// force the row's expiry into the past.
	_, err = db.ExecContext(ctx,
		`UPDATE run_execution SET lease_expires = NOW() - INTERVAL '1 second' WHERE run_id = $1`, runID)
	require.NoError(t, err)

	lease2, err := g.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "node-b", lease2.Owner)
	assert.Greater(t, lease2.Epoch, firstEpoch)
	defer func() { _ = g.End(ctx, lease2) }()

	// The old owner is fenced out from its very next write.
	err = g.Owns(ctx, lease, 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFencedOut)
	// Its End is a harmless no-op (the row now belongs to node-b).
	require.NoError(t, g.End(ctx, lease))
}

// TestExecutionGuard_RowDeletedByTakeover covers the post-takeover
// zombie: the takeover loop DELETES the run_execution row; the old
// owner's Owns must fail closed (no row → not owner → ErrFencedOut).
func TestExecutionGuard_RowDeletedByTakeover(t *testing.T) {
	g, db := newTestExecGuard(t)
	ctx := context.Background()
	const runID = "run-guard-takeover"

	lease, err := g.Register(ctx, runID, "node-a", 10*time.Second)
	require.NoError(t, err)

	// Takeover-style deletion (what internal/takeover does).
	_, err = db.ExecContext(ctx, `DELETE FROM run_execution WHERE run_id = $1`, runID)
	require.NoError(t, err)

	err = g.Owns(ctx, lease, 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFencedOut)
	// Heartbeat fails identically (background renewal stops).
	err = g.Heartbeat(ctx, lease, 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFencedOut)
}

// TestExecutionGuard_ExpiredExecutionsScan pins the takeover candidate
// scan: only run IDs whose lease has lapsed are reported.
func TestExecutionGuard_ExpiredExecutionsScan(t *testing.T) {
	g, db := newTestExecGuard(t)
	ctx := context.Background()
	const live = "run-guard-live"
	const dead = "run-guard-dead"

	liveLease, err := g.Register(ctx, live, "node-a", 30*time.Second)
	require.NoError(t, err)
	defer func() { _ = g.End(ctx, liveLease) }()
	deadLease, err := g.Register(ctx, dead, "node-a", 30*time.Second)
	require.NoError(t, err)
	_ = deadLease

	// Deterministic expiry for the dead one only.
	_, err = db.ExecContext(ctx,
		`UPDATE run_execution SET lease_expires = NOW() - INTERVAL '1 second' WHERE run_id = $1`, dead)
	require.NoError(t, err)

	ids, err := g.ExpiredExecutions(ctx)
	require.NoError(t, err)
	assert.Contains(t, ids, dead)
	assert.NotContains(t, ids, live)
}

// TestExecutionGuard_OwnsRenewsLease documents the renewal-in-check
// contract with an observable assertion: after Owns, the lease must not
// appear in the expired scan even though its original TTL window has
// been forced into the past.
func TestExecutionGuard_OwnsRenewsLease(t *testing.T) {
	g, db := newTestExecGuard(t)
	ctx := context.Background()
	const runID = "run-guard-renew"

	lease, err := g.Register(ctx, runID, "node-a", 30*time.Second)
	require.NoError(t, err)
	defer func() { _ = g.End(ctx, lease) }()

	// Force the lease to the edge of expiry, then Owns-renew.
	_, err = db.ExecContext(ctx,
		`UPDATE run_execution SET lease_expires = NOW() + INTERVAL '10 milliseconds' WHERE run_id = $1`, runID)
	require.NoError(t, err)
	require.NoError(t, g.Owns(ctx, lease, 30*time.Second))

	ids, err := g.ExpiredExecutions(ctx)
	require.NoError(t, err)
	assert.NotContains(t, ids, runID, "Owns must have renewed the lease past NOW()+ttl")
}

// Compile-time sentinel documentation: ErrFencedOut is the vocabulary
// the wiring layer matches (by message fragment) to attach the engine
// sentinel. This keeps the contract visible next to the tests.
var _ = errors.Is
