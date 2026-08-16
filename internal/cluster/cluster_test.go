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