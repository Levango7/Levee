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
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)

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
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)

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
	loop := NewLoop(mgr, store, "node-b", time.Second, 4, DefaultClaimTimeout)

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
	loop := NewLoop(mgr, store, "node-leader", time.Second, 2, DefaultClaimTimeout)

	claimed, err := loop.DispatchOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, claimed, "dispatch must respect worker capacity")
}

func TestDispatchOnce_ReclaimsTerminalAssignment(t *testing.T) {
	store := newDispatchStore(t)
	mgr := localLeaderManager(t, "node-leader")
	seedApprovedRun(t, store, "run-d4")
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)

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
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)

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
	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)
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

// --- D-4: stale-assignment reclaim -------------------------------------------

// twoNodeManager joins a leader (this process) and an idle worker. The
// heartbeat timeout is far beyond the test duration so the worker — which
// never heartbeats on its own — stays an active member.
func twoNodeManager(t *testing.T, leaderID, workerID string) *cluster.ClusterManager {
	t.Helper()
	mgr := cluster.NewClusterManager(nil, cluster.ManagerConfig{
		SelfID:           leaderID,
		HeartbeatTimeout: time.Hour,
	})
	require.NoError(t, mgr.Join(cluster.Node{ID: leaderID, Address: "127.0.0.1:1", Role: cluster.RoleMaster, Status: cluster.StatusActive}))
	require.NoError(t, mgr.Join(cluster.Node{ID: workerID, Address: "127.0.0.1:2", Role: cluster.RoleWorker, Status: cluster.StatusActive}))
	require.NoError(t, mgr.Start(context.Background()))
	t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
	mgr.HealthCheck()
	require.Eventually(t, func() bool {
		leader, ok := mgr.GetLeader()
		return ok && leader.ID == leaderID
	}, 2*time.Second, 10*time.Millisecond, "node %s must be elected leader", leaderID)
	return mgr
}

// seedPending creates a pending assignment and ages its timestamps by age.
// The store stamps rows with its own clock, so the age is applied with a
// direct UPDATE rather than through CreateAssignment.
func seedPending(t *testing.T, store *state.SQLiteStore, runID, owner string, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.CreateAssignment(ctx, &state.Assignment{
		RunID: runID, OwnerNode: owner, Epoch: 1, State: state.AssignStatePending,
	}))
	aged := time.Now().UTC().Add(-age)
	_, err := store.DB().ExecContext(ctx,
		`UPDATE run_assignment SET assigned_at=?, updated_at=? WHERE run_id=?`, aged, aged, runID)
	require.NoError(t, err)
}

func TestReclaimOnce_ReassignsStalePendingToLiveWorker(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedApprovedRun(t, store, "run-stale")
	seedPending(t, store, "run-stale", "node-dead", 2*time.Hour)

	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)

	reclaimed, err := loop.ReclaimOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, reclaimed, "a pending assignment past the claim timeout must be reclaimed")

	a, err := store.GetAssignment(ctx, "run-stale")
	require.NoError(t, err)
	assert.Equal(t, int64(2), a.Epoch, "reclaim bumps the epoch to fence the stale owner out")
	assert.NotEqual(t, "node-dead", a.OwnerNode, "the assignment moves off the failed owner")
	assert.Equal(t, state.AssignStatePending, a.State)
	assert.Equal(t, "", a.Result, "reclaim clears any stale result")
}
func TestReclaimOnce_LateClaimIsFencedOut(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedApprovedRun(t, store, "run-late")
	seedPending(t, store, "run-late", "node-dead", 2*time.Hour)

	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)
	reclaimed, err := loop.ReclaimOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed)

	// The slow worker wakes up and claims with the epoch it read before the
	// reclaim: fenced out, so the run cannot execute twice.
	ok, err := store.UpdateAssignmentStateIf(ctx, "run-late", 1, state.AssignStatePending, state.AssignmentStateExecuting)
	require.NoError(t, err)
	assert.False(t, ok, "a late claim against the pre-reclaim epoch must fail")

	a, err := store.GetAssignment(ctx, "run-late")
	require.NoError(t, err)
	ok, err = store.UpdateAssignmentStateIf(ctx, "run-late", a.Epoch, state.AssignStatePending, state.AssignmentStateExecuting)
	require.NoError(t, err)
	assert.True(t, ok, "the current owner can still claim its assignment")
}

func TestReclaimOnce_LeavesFreshPendingAlone(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedApprovedRun(t, store, "run-fresh")
	seedPending(t, store, "run-fresh", "node-worker", time.Second)

	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)
	reclaimed, err := loop.ReclaimOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, reclaimed, "a pending assignment inside the claim timeout is untouched")

	a, err := store.GetAssignment(ctx, "run-fresh")
	require.NoError(t, err)
	assert.Equal(t, "node-worker", a.OwnerNode)
	assert.Equal(t, int64(1), a.Epoch)
}

func TestReclaimOnce_TerminalisesOrphanForNonApprovedRun(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedApprovedRun(t, store, "run-orphan")
	seedPending(t, store, "run-orphan", "node-dead", 2*time.Hour)
	// The run left the dispatchable state (paused, or re-planned back to
	// draft) while its assignment was still pending.
	_, err := store.UpdateRunStatusIf(ctx, "run-orphan", "approved", "paused", time.Now().UTC())
	require.NoError(t, err)

	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)
	reclaimed, err := loop.ReclaimOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, reclaimed, "orphans are terminalised, never handed out")

	a, err := store.GetAssignment(ctx, "run-orphan")
	require.NoError(t, err)
	assert.Equal(t, state.AssignmentStateInterrupted, a.State)
	assert.Equal(t, int64(1), a.Epoch, "terminalising keeps the epoch; a later claim bumps it via Reassign")
}
func TestWorkerLoads_CountsPendingAndExecuting(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedPending(t, store, "load-pending", "node-worker", time.Second)
	now := time.Now().UTC()
	require.NoError(t, store.CreateAssignment(ctx, &state.Assignment{
		RunID: "load-executing", OwnerNode: "node-worker", Epoch: 1, State: state.AssignmentStateExecuting,
		AssignedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateAssignment(ctx, &state.Assignment{
		RunID: "load-done", OwnerNode: "node-worker", Epoch: 1, State: state.AssignmentStateDone,
		AssignedAt: now, UpdatedAt: now,
	}))

	loop := NewLoop(mgr, store, "node-leader", time.Second, 4, DefaultClaimTimeout)
	loads, err := loop.workerLoads(ctx)
	require.NoError(t, err)
	require.Len(t, loads, 2)
	byID := map[string]int{}
	for _, w := range loads {
		byID[w.id] = w.active
	}
	assert.Equal(t, 2, byID["node-worker"],
		"pending assignments are commitments: only terminal rows are excluded from load")
	assert.Equal(t, 0, byID["node-leader"])
}

func TestDispatchOnce_PendingCountsAgainstCapacity(t *testing.T) {
	ctx := context.Background()
	store := newDispatchStore(t)
	mgr := twoNodeManager(t, "node-leader", "node-worker")
	seedApprovedRun(t, store, "run-cap-d4")
	// Both nodes are already committed to one unclaimed run each.
	seedPending(t, store, "hold-leader", "node-leader", time.Second)
	seedPending(t, store, "hold-worker", "node-worker", time.Second)

	// capacity 1 -> both nodes are saturated by their pending commitments, so
	// the new run must NOT be dispatched. Before D-4 only executing rows
	// counted, both nodes looked idle, and the run was oversubscribed.
	loop := NewLoop(mgr, store, "node-leader", time.Second, 1, DefaultClaimTimeout)
	claimed, err := loop.DispatchOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, claimed, "pending commitments must count against worker capacity")

	a, err := store.GetAssignment(ctx, "run-cap-d4")
	require.NoError(t, err)
	assert.Nil(t, a, "no assignment may be created for the saturated cluster")
}
