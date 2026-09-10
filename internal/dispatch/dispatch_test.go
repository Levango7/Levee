// dispatch_test.go exercises the cross-node scheduling loop against a real
// SQLite store (no clock dependency: DispatchOnce is the tested seam). The
// cluster manager runs in in-process mode (nil db) and converges leadership
// through its background health loop — tests wait for that convergence.
package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localLeaderManager builds a ClusterManager whose leadership converges on a
// single local node (nil db → in-process only mode). Leadership is triggered
// deterministically via HealthCheck() rather than waiting for the 10s ticker.
func localLeaderManager(t *testing.T, nodeID string) *cluster.ClusterManager {
	t.Helper()
	mgr := cluster.NewClusterManager(nil, cluster.ManagerConfig{SelfID: nodeID})
	require.NoError(t, mgr.Join(cluster.Node{ID: nodeID, Address: "127.0.0.1:0", Role: cluster.RoleMaster, Status: cluster.StatusActive}))
	require.NoError(t, mgr.Start(context.Background()))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
	// Deterministically trigger the election: with a single active node,
	// HealthCheck → MarkStale → elect converges immediately.
	mgr.HealthCheck()
	require.Eventually(t, func() bool {
		leader, ok := mgr.GetLeader()
		return ok && leader.ID == nodeID
	}, 2*time.Second, 10*time.Millisecond, "node %s must be elected leader", nodeID)
	return mgr
}

func seedApprovedRun(t *testing.T, store state.Store, runID string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: runID, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", Status: "approved", ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
}

func TestDispatchOnce_ClaimsApprovedRun(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-d1")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4)

	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, claimed)

	a, err := store.GetAssignment(context.Background(), "run-d1")
	require.NoError(t, err)
	require.NotNil(t, a)
	assert.Equal(t, "node-leader", a.OwnerNode)
	assert.Equal(t, int64(1), a.Epoch)
	assert.Equal(t, state.AssignStatePending, a.State)
}

func TestDispatchOnce_SkipsRunWithActiveAssignment(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-d2")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4)

	_, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)

	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed, "a run with an active assignment must not be re-dispatched")
}

func TestDispatchOnce_NonLeaderDoesNothing(t *testing.T) {
	store := newDispatchStore(t)
	// Leader is node-a; we act as node-b.
	mgr := localLeaderManager(t, "node-a")
	seedApprovedRun(t, store, "run-d3")
	loop := NewLoop(mgr, store, "node-b", time.Second, 4)

	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, claimed, "a non-leader must not dispatch runs")
}

func TestDispatchOnce_WorkerCapacityLimitsClaims(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	for i := 0; i < 5; i++ {
		seedApprovedRun(t, store, fmt.Sprintf("run-cap-%d", i))
	}
	loop := NewLoop(mgr, store, "node-leader", time.Second, 2)

	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, claimed, "dispatch must respect worker capacity")
}

func TestDispatchOnce_ReclaimsTerminalAssignment(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-d4")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4)

	_, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	a1, err := store.GetAssignment(context.Background(), "run-d4")
	require.NoError(t, err)
	require.Equal(t, int64(1), a1.Epoch)

	// Simulate terminal outcome (run completed).
	_, err = store.UpdateAssignmentStateIf(context.Background(), "run-d4", 1, state.AssignStatePending, state.AssignmentStateDone)
	require.NoError(t, err)

	// Re-dispatch should reclaim with bumped epoch.
	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, claimed)

	a2, err := store.GetAssignment(context.Background(), "run-d4")
	require.NoError(t, err)
	assert.Equal(t, int64(2), a2.Epoch, "reclaim must bump the epoch")
}

func TestDispatchOnce_ConcurrentDispatchersSerialised(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-race")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4)

	var wg sync.WaitGroup
	var mu sync.Mutex
	totalClaimed := 0
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := loop.DispatchOnce(context.Background())
			require.NoError(t, err)
			mu.Lock()
			totalClaimed += claimed
			mu.Unlock()
		}()
	}
	wg.Wait()

	assert.Equal(t, 1, totalClaimed, "exactly one dispatcher must claim the run")
	a, err := store.GetAssignment(context.Background(), "run-race")
	require.NoError(t, err)
	assert.Equal(t, int64(1), a.Epoch)
}

func TestMarkDone_IdempotentAndTolerant(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-done")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4)
	_, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)

	// MarkDone twice: second is a no-op.
	require.NoError(t, loop.MarkDone(context.Background(), "run-done", state.AssignResultCompleted))
	require.NoError(t, loop.MarkDone(context.Background(), "run-done", state.AssignResultCompleted))

	a, err := store.GetAssignment(context.Background(), "run-done")
	require.NoError(t, err)
	assert.Equal(t, state.AssignmentStateDone, a.State)
	assert.Equal(t, state.AssignResultCompleted, a.Result)
}

// newDispatchStore returns a fresh file-backed SQLite store for dispatch tests.
func newDispatchStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dispatch-test.db")
	store, err := state.NewSQLiteStore(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}
