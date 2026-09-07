// takeover_test.go exercises the takeover loop against a real
// PostgreSQL instance (skipped unless LEVEE_PG_TEST_DSN is set — the CI
// integration&postgres job provides it). Every test drives TakeoverOnce
// directly and manufactures lease expiry with a targeted UPDATE — no
// sleeps, no clock races: determinism is a design requirement (§4-5).
package takeover

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/nexus/levee/internal/cluster"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// takeoverEnv is a full cluster stack: PG store, two cluster managers
// (leader "node-a" and follower "node-b" converge via the shared table),
// an execution guard and a takeover loop driven by node-a.
type takeoverEnv struct {
	db    *sql.DB
	store *state.PGStore
	guard *cluster.ExecutionGuard
	loopy *Loop
	mgrA  *cluster.ClusterManager
	mgrB  *cluster.ClusterManager
}

func newTakeoverEnv(t *testing.T) *takeoverEnv {
	t.Helper()
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL takeover test")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, cluster.EnsureClusterSchemaForTest(context.Background(), db))

	store, err := state.NewPGStore(context.Background(), dsn, state.PGPoolConfig{
		MaxOpenConns: 5,
		MaxIdleConns: 2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// Clean slate: these tests own their rows. TRUNCATE (not DELETE —
	// the WORM trigger rejects row deletes on trace) with CASCADE so a
	// previous run's leftover rows can never collide. cluster_nodes is
	// deliberately NOT truncated: the state and cluster package test
	// binaries run concurrently against this database and own rows there.
	_, err = db.ExecContext(context.Background(), `
TRUNCATE TABLE trace, steps, batches, runs, run_execution, cluster_locks RESTART IDENTITY CASCADE`)
	require.NoError(t, err)

	joinNode := func(id string) *cluster.ClusterManager {
		mgr := cluster.NewClusterManager(db, cluster.ManagerConfig{SelfID: id})
		require.NoError(t, mgr.Join(cluster.Node{ID: id, Address: "127.0.0.1:0", Role: cluster.RoleMaster, Status: cluster.StatusActive}))
		require.NoError(t, mgr.Start(context.Background()))
		t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
		return mgr
	}
	env := &takeoverEnv{
		db:    db,
		store: store,
		guard: cluster.NewExecutionGuard(db),
		mgrA:  joinNode("node-a"),
		mgrB:  joinNode("node-b"),
	}

	// Leadership convergence. A single sync round is NOT enough: each
	// manager's first SyncFromPeers folds the shared table into a local
	// registry that may still be missing the other node's row (join and
	// sync race), and electLeaderLocked then elects on the partial view —
	// node-b can end up believing IT is the leader (the smallest active
	// master in a registry containing only itself). The background loop
	// would converge eventually; tests cannot wait on wall-clock, so we
	// drive sync rounds and assert convergence on the OBSERVABLE (both
	// registries reporting the same leader, which must be node-a).
	require.Eventually(t, func() bool {
		_ = env.mgrA.SyncOnceForTest(context.Background())
		_ = env.mgrB.SyncOnceForTest(context.Background())
		la, okA := env.mgrA.GetLeader()
		lb, okB := env.mgrB.GetLeader()
		return okA && okB && la.ID == "node-a" && lb.ID == "node-a"
	}, 10*time.Second, 20*time.Millisecond, "both nodes must converge on node-a as leader before the test proper")

	env.loopy = NewLoop(env.mgrA, env.guard, store, "node-a", time.Second)
	return env
}

// expireLease forces the run's lease into the past deterministically.
func (e *takeoverEnv) expireLease(t *testing.T, runID string) {
	t.Helper()
	_, err := e.db.ExecContext(context.Background(),
		`UPDATE run_execution SET lease_expires = NOW() - INTERVAL '1 second' WHERE run_id = $1`, runID)
	require.NoError(t, err)
}

// seedRunningRun creates a run row directly in the running state.
func (e *takeoverEnv) seedRunningRun(t *testing.T, runID string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, e.store.CreateRun(context.Background(), &state.Run{
		ID: runID, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", Status: "running", ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
}

func (e *takeoverEnv) tracesOf(t *testing.T, runID string, event string) []*state.Trace {
	t.Helper()
	traces, err := e.store.ListTraces(context.Background(), state.TraceFilter{RunID: runID, Event: event})
	require.NoError(t, err)
	return traces
}

// TestTakeover_LeaderSettlesExpiredRun: the headline path — expired
// lease + running run → interrupted + audit trace + execution row gone.
func TestTakeover_LeaderSettlesExpiredRun(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-tk-1"

	env.seedRunningRun(t, runID)
	_, err := env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	// node-a (leader) sweeps.
	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Settled, runID)

	run, err := env.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status)

	traces := env.tracesOf(t, runID, "interrupted_by_takeover")
	assert.Len(t, traces, 1, "the takeover must be audited on the run's trace chain")
	assert.Equal(t, "cluster-takeover", traces[0].Actor)

	lease, err := env.guard.GetExecution(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, lease, "the execution row must die with the settlement")
}

// TestTakeover_NonRunningRunSkipped: an expired lease whose run already
// settled elsewhere (terminal) is left alone — the status CAS stands down.
func TestTakeover_NonRunningRunSkipped(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-tk-2"

	env.seedRunningRun(t, runID)
	_, err := env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	// Someone else finished the run between expiry and the sweep.
	ok, err := env.store.UpdateRunStatusIf(ctx, runID, "running", "completed", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, ok)

	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Skipped, runID)
	assert.NotContains(t, res.Settled, runID)

	run, err := env.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status, "the winner's terminal state must be untouched")
	assert.Empty(t, env.tracesOf(t, runID, "interrupted_by_takeover"))
}

// TestTakeover_NonLeaderDoesNotAct: node-b's loop (follower) sees the
// expired candidate but performs no transition.
func TestTakeover_NonLeaderDoesNotAct(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-tk-3"

	env.seedRunningRun(t, runID)
	_, err := env.guard.Register(ctx, runID, "node-a", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	followerLoop := NewLoop(env.mgrB, env.guard, env.store, "node-b", time.Second)
	res, err := followerLoop.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Empty(t, res.Settled, "the follower must never settle runs")

	run, err := env.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "running", run.Status)
}

// TestTakeover_SweepIsIdempotent: a second sweep over the same expired
// run settles nothing new (the run is already terminal, the row gone).
func TestTakeover_SweepIsIdempotent(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-tk-4"

	env.seedRunningRun(t, runID)
	_, err := env.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	env.expireLease(t, runID)

	_, err = env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.NotContains(t, res.Settled, runID)
	assert.NotContains(t, res.Skipped, runID, "a settled run has no execution row left to scan")

	traces := env.tracesOf(t, runID, "interrupted_by_takeover")
	assert.Len(t, traces, 1, "exactly one takeover trace across repeated sweeps")
}

// TestTakeover_LiveLeaseNotTouched: a fresh (unexpired) lease is never a
// candidate — healthy executions are invisible to the loop.
func TestTakeover_LiveLeaseNotTouched(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()
	const runID = "run-tk-5"

	env.seedRunningRun(t, runID)
	_, err := env.guard.Register(ctx, runID, "node-b", time.Minute)
	require.NoError(t, err)

	res, err := env.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.NotContains(t, res.Settled, runID)

	run, err := env.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "running", run.Status)
}

// TestTakeover_StartStopLifecycle covers the loop plumbing without
// depending on the ticker firing (TakeoverOnce is the tested path).
func TestTakeover_StartStopLifecycle(t *testing.T) {
	env := newTakeoverEnv(t)
	ctx := context.Background()

	require.NoError(t, env.loopy.Start(ctx))
	// Idempotent start.
	require.NoError(t, env.loopy.Start(ctx))
	require.NoError(t, env.loopy.Stop(context.Background()))
	// Idempotent stop.
	require.NoError(t, env.loopy.Stop(context.Background()))
}
