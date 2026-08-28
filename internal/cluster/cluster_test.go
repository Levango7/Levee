// cluster_test.go exercises the ClusterManager lifecycle. Tests that require a
// real PostgreSQL instance are skipped unless LEVEE_PG_TEST_DSN is set.

package cluster

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestDB opens a *sql.DB for the test PostgreSQL instance. Caller must
// close the returned handle.
func openTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	db := stdlib.OpenDB(*cfg)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("ping postgres: %v", err)
	}
	return db
}

// TestClusterManagerStartStop exercises the lifecycle in nil-db mode (no
// health-check against PostgreSQL, but the loop still runs).
func TestClusterManagerStartStop(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{
		HealthCheckInterval: 50 * time.Millisecond,
		HeartbeatTimeout:    200 * time.Millisecond,
		SelfID:              "self",
	})
	require.NoError(t, mgr.Join(Node{ID: "self", Address: "localhost:1", Role: RoleMaster}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	// Wait a couple of ticks so the loop refreshes our heartbeat.
	time.Sleep(150 * time.Millisecond)

	// Self heartbeat should have been refreshed.
	n, ok := mgr.Registry().Get("self")
	require.True(t, ok)
	assert.True(t, time.Since(n.LastHeartbeat) < 200*time.Millisecond)

	// Stop the manager.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}

// TestClusterManagerStartIdempotent verifies Start is idempotent.
func TestClusterManagerStartIdempotent(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{SelfID: "x"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, mgr.Start(ctx))
	require.NoError(t, mgr.Start(ctx)) // no-op
	require.NoError(t, mgr.Stop(context.Background()))
}

// TestClusterManagerStopIdempotent verifies Stop is idempotent.
func TestClusterManagerStopIdempotent(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{})
	// Stop before Start is a no-op.
	require.NoError(t, mgr.Stop(context.Background()))
}

// TestClusterManagerJoinLeave exercises Join/Leave.
func TestClusterManagerJoinLeave(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{})
	require.NoError(t, mgr.Join(Node{ID: "n1", Address: "a", Role: RoleWorker}))
	require.NoError(t, mgr.Join(Node{ID: "n2", Address: "b", Role: RoleWorker}))
	assert.Equal(t, 2, mgr.Registry().Count())
	require.NoError(t, mgr.Leave("n1"))
	assert.Equal(t, 1, mgr.Registry().Count())
}

// TestClusterManagerHealthCheck exercises a single sweep.
func TestClusterManagerHealthCheck(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{
		HeartbeatTimeout: 100 * time.Millisecond,
	})
	require.NoError(t, mgr.Join(Node{ID: "n1", Address: "a"}))
	require.NoError(t, mgr.Join(Node{ID: "n2", Address: "b"}))

	// Make n2 stale.
	mgr.Registry().mu.Lock()
	mgr.Registry().nodes["n2"].LastHeartbeat = time.Now().UTC().Add(-1 * time.Second)
	mgr.Registry().mu.Unlock()

	result := mgr.HealthCheck()
	assert.Contains(t, result.StaleNodes, "n2")
	assert.Equal(t, 2, result.Total)
	assert.Equal(t, 1, result.Active)
}

// TestClusterManagerSelfHeartbeat checks the explicit heartbeat helper.
func TestClusterManagerSelfHeartbeat(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{SelfID: "self"})
	require.NoError(t, mgr.Join(Node{ID: "self", Address: "a"}))
	require.NoError(t, mgr.SelfHeartbeat())
	n, ok := mgr.Registry().Get("self")
	require.True(t, ok)
	assert.False(t, n.LastHeartbeat.IsZero())
}

// TestClusterManagerSelfHeartbeatNoSelfID checks the error path.
func TestClusterManagerSelfHeartbeatNoSelfID(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{})
	assert.Error(t, mgr.SelfHeartbeat())
}

// TestClusterManagerEnsureStarted checks the guard.
func TestClusterManagerEnsureStarted(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{})
	assert.Error(t, mgr.EnsureStarted())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, mgr.Start(ctx))
	assert.NoError(t, mgr.EnsureStarted())
	require.NoError(t, mgr.Stop(context.Background()))
}

// TestClusterManagerGetNodesGetLeader checks the snapshot helpers.
func TestClusterManagerGetNodesGetLeader(t *testing.T) {
	mgr := NewClusterManager(nil, ManagerConfig{})
	require.NoError(t, mgr.Join(Node{ID: "m1", Address: "a", Role: RoleMaster}))
	require.NoError(t, mgr.Join(Node{ID: "w1", Address: "b", Role: RoleWorker}))

	nodes := mgr.GetNodes()
	assert.Len(t, nodes, 2)

	leader, ok := mgr.GetLeader()
	require.True(t, ok)
	assert.Equal(t, "m1", leader.ID)
}

// TestClusterPGTwoNodeVisibility exercises the shared PostgreSQL registry:
// two managers backed by the same database see each other, and a node that
// stops heartbeating (crash simulation) is marked offline by its peer.
func TestClusterPGTwoNodeVisibility(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL cluster test")
	}
	db := openTestDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, ensureClusterSchema(ctx, db))
	// Isolate from rows left by earlier runs of this or other suites.
	_, err := db.ExecContext(ctx, `DELETE FROM cluster_nodes`)
	require.NoError(t, err)

	cfgA := ManagerConfig{SelfID: "node-a", HealthCheckInterval: 100 * time.Millisecond, HeartbeatTimeout: 500 * time.Millisecond}
	cfgB := ManagerConfig{SelfID: "node-b", HealthCheckInterval: 100 * time.Millisecond, HeartbeatTimeout: 500 * time.Millisecond}
	mgrA := NewClusterManager(db, cfgA)
	mgrB := NewClusterManager(db, cfgB)

	require.NoError(t, mgrA.Join(Node{ID: "node-a", Address: "a:9090", Role: RoleMaster}))
	require.NoError(t, mgrB.Join(Node{ID: "node-b", Address: "b:9090", Role: RoleWorker}))

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, mgrA.Start(runCtx))
	require.NoError(t, mgrB.Start(runCtx))

	// A must discover B through the shared table.
	assert.Eventually(t, func() bool {
		n, ok := mgrA.Registry().Get("node-b")
		return ok && n.Status == StatusActive
	}, 5*time.Second, 50*time.Millisecond, "node-a never saw node-b as active")

	// Leader converges to the master role on both sides.
	leaderA, okA := mgrA.GetLeader()
	leaderB, okB := mgrB.GetLeader()
	if assert.True(t, okA) && assert.True(t, okB) {
		assert.Equal(t, "node-a", leaderA.ID)
		assert.Equal(t, "node-a", leaderB.ID)
	}

	// Simulate a crash of B: stop its loop WITHOUT Leave, so the node row
	// stays in the table with an ageing heartbeat.
	stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
	defer stopCancel()
	require.NoError(t, mgrB.Stop(stopCtx))

	// A must mark B offline once the heartbeat ages past the timeout.
	assert.Eventually(t, func() bool {
		n, ok := mgrA.Registry().Get("node-b")
		return ok && n.Status == StatusOffline
	}, 5*time.Second, 50*time.Millisecond, "node-a never marked node-b offline")

	// Graceful leave removes the row entirely.
	require.NoError(t, mgrA.Leave("node-b"))
	assert.Eventually(t, func() bool {
		_, ok := mgrA.Registry().Get("node-b")
		return !ok
	}, 5*time.Second, 50*time.Millisecond, "node-b row still visible after leave")

	require.NoError(t, mgrA.Stop(stopCtx))
}

// TestClusterPGStaleLockSweep verifies the health loop releases lock leases
// whose holder stopped refreshing (crash simulation).
func TestClusterPGStaleLockSweep(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL cluster test")
	}
	db := openTestDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	require.NoError(t, ensureClusterSchema(ctx, db))

	// A "dead" node grabs a lock with a short lease and never refreshes.
	dead := NewDistributedLockManager(db)
	key := "test:cluster:sweep:" + time.Now().Format("150405.000000000")
	_, err := dead.Acquire(ctx, key, "dead-node", 500*time.Millisecond)
	require.NoError(t, err)

	// A live manager whose health loop sweeps stale leases.
	mgr := NewClusterManager(db, ManagerConfig{
		SelfID:              "node-a",
		HealthCheckInterval: 100 * time.Millisecond,
		HeartbeatTimeout:    time.Minute,
	})
	require.NoError(t, mgr.Join(Node{ID: "node-a", Address: "a:9090"}))
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	require.NoError(t, mgr.Start(runCtx))

	// The sweep must remove the expired lease, after which the key is free.
	assert.Eventually(t, func() bool {
		_, err := mgr.Locks().Acquire(ctx, key, "node-a", time.Minute)
		if err != nil {
			return false
		}
		return true
	}, 5*time.Second, 100*time.Millisecond, "expired lease was never swept")
	require.NoError(t, mgr.Locks().Release(ctx, key, "node-a"))

	stopCtx, stopCancel := context.WithTimeout(ctx, 2*time.Second)
	defer stopCancel()
	require.NoError(t, mgr.Stop(stopCtx))
}
