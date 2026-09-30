//go:build integration

// takeover_e2e_test.go is the design-B acceptance suite (§4 of
// docs/design-cluster-failover.md), executed against a real PostgreSQL:
// two full cluster stacks (node registries + wiring engines sharing the
// loopback channel registry stand-in for ssh) execute changes across
// nodes, and the takeover loop settles crashed executors.
//
// DEATH SIMULATION EQUIVALENCE NOTE (§7.4-Q2): the executor "death" is
// simulated in-process by (a) a loopback channel whose Exec blocks on a
// release gate and (b) forcibly expiring the run_execution lease with a
// targeted UPDATE. The observables every assertion reads — the
// run_execution row, the run status, the step rows and the audit traces
// — are byte-identical to what a kill -9 of the executor process leaves
// behind (in both cases the lease is simply never renewed again); the
// only difference is process residue, which no assertion observes. Real
// subprocess kills were deliberately rejected: process-reaping races on
// shared CI runners are the exact flakiness this project just finished
// eliminating.
package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/cluster"
	leveegrpc "github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/takeover"
	"github.com/nexus/levee/internal/wiring"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// takeCluster is one node's full stack.
type takeCluster struct {
	nodeID  string
	db      *sql.DB
	store   *state.PGStore
	mgr     *cluster.ClusterManager
	guard   *cluster.ExecutionGuard
	engine  *wiring.Engine
	adapter *leveegrpc.EngineAdapter
}

// takeHarness boots two nodes (node-a = leader, node-b) over one real
// PostgreSQL, with one shared blocking loopback channel registry.
type takeHarness struct {
	db     *sql.DB
	store  *state.PGStore
	guard  *cluster.ExecutionGuard
	rec    *loopRecorder
	a, b   takeCluster
	loopy  *takeover.Loop
	loopyB *takeover.Loop
}

func newTakeHarness(t *testing.T) *takeHarness {
	t.Helper()
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping takeover e2e (integration & postgres job provides it)")
	}
	h := &takeHarness{}

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	h.db = db
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, cluster.EnsureClusterSchemaForTest(context.Background(), db))

	store, err := state.NewPGStore(context.Background(), dsn, state.PGPoolConfig{
		MaxOpenConns: 5, MaxIdleConns: 2,
	})
	require.NoError(t, err)
	h.store = store
	h.guard = cluster.NewExecutionGuard(db)
	t.Cleanup(func() { _ = store.Close() })

	// Shared loopback registry: blocking gate + failure injection.
	h.rec = &loopRecorder{
		release: make(chan struct{}),
		entered: make(chan string, 16),
	}
	reg := channel.NewChannelRegistry()
	reg.Register("local", loopFactory{rec: h.rec})

	// Clean slate for this test's tables (cluster_nodes left alone: both
	// nodes join below and leave via Stop).
	_, err = db.ExecContext(context.Background(), `DELETE FROM run_execution`)
	require.NoError(t, err)

	boot := func(nodeID string) takeCluster {
		mgr := cluster.NewClusterManager(db, cluster.ManagerConfig{SelfID: nodeID})
		require.NoError(t, mgr.Join(cluster.Node{ID: nodeID, Address: "127.0.0.1:0", Role: cluster.RoleMaster, Status: cluster.StatusActive}))
		require.NoError(t, mgr.Start(context.Background()))
		t.Cleanup(func() { _ = mgr.Stop(context.Background()) })
		require.NoError(t, mgr.SyncOnceForTest(context.Background()))
		eng := wiring.NewEngine(store,
			wiring.WithChannelRegistry(reg),
			wiring.WithExecutionGuard(&directGuard{g: h.guard, node: nodeID, ttl: 15 * time.Second}, nodeID),
			wiring.WithExecLeaseTTL(15*time.Second),
		)
		return takeCluster{nodeID: nodeID, db: db, store: store, mgr: mgr, guard: h.guard, engine: eng, adapter: eng.Adapter()}
	}
	h.a = boot("node-a")
	h.b = boot("node-b")

	// Converge leadership deterministically: refresh BOTH views so both
	// registries agree on the leader (smallest active master ID = node-a).
	require.NoError(t, h.a.mgr.SyncOnceForTest(context.Background()))
	require.NoError(t, h.b.mgr.SyncOnceForTest(context.Background()))

	h.loopy = takeover.NewLoop(h.a.mgr, h.guard, store, "node-a", time.Second)
	h.loopyB = takeover.NewLoop(h.b.mgr, h.guard, store, "node-b", time.Second)
	return h
}

// directGuard adapts the concrete cluster guard to the wiring interface
// (mirrors cmd_serve.go's adapter).
type directGuard struct {
	g    *cluster.ExecutionGuard
	node string
	ttl  time.Duration
}

func (d *directGuard) Begin(ctx context.Context, runID string) (wiring.ExecutionLease, error) {
	lease, err := d.g.Register(ctx, runID, d.node, d.ttl)
	if err != nil {
		return nil, err
	}
	return d.g.LeaseGuard(lease, d.ttl), nil
}

const takeWorkflowYAML = `name: take-e2e
target:
  type: host
  query: "env=test"
steps:
  - name: work
    action: shell.exec
    args:
      cmd: work-command
    rollback:
      steps:
        - name: undo-work
          action: shell.exec
          args:
            cmd: undo-command
  - name: more
    action: shell.exec
    args:
      cmd: more-command
`

// seedTakeRun creates a run row with the given status and workflow.
func (h *takeHarness) seedTakeRun(t *testing.T, runID, status string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, h.store.CreateRun(context.Background(), &state.Run{
		ID: runID, WorkflowName: takeWorkflowYAML, TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", Status: status, ApprovalStatus: "approved",
		CreatedAt: now, UpdatedAt: now, Creator: "test",
	}))
	require.NoError(t, h.store.UpsertTarget(context.Background(), &state.Target{
		ID: "tgt-web-1", Hostname: "web-1", Port: 22, ChannelType: "local",
		Status: "active", CreatedAt: now,
	}))
	// Persist a plan artifact so the engine's stored-plan gate passes.
	p := &plan.Plan{
		ID: "plan-" + runID, WorkflowName: takeWorkflowYAML,
		Batches: []plan.Batch{{
			Index: 0, Targets: []string{"web-1"}, MaxConcurrency: 1,
			Steps: []plan.PlanStep{
				{Name: "work", Module: "shell", Action: "exec", Args: map[string]any{"cmd": "work-command"}},
				{Name: "more", Module: "shell", Action: "exec", Args: map[string]any{"cmd": "more-command"}},
			},
		}},
		TotalTargets: 1, CreatedAt: now,
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	run, err := h.store.GetRun(context.Background(), runID)
	require.NoError(t, err)
	run.PlanJSON = string(raw)
	run.PlanHash = plan.ComputeHash(p)
	require.NoError(t, h.store.UpdateRun(context.Background(), run))
}

// expireTakeLease forces the run's lease into the past (deterministic).
func (h *takeHarness) expireTakeLease(t *testing.T, runID string) {
	t.Helper()
	_, err := h.db.ExecContext(context.Background(),
		`UPDATE run_execution SET lease_expires = NOW() - INTERVAL '1 second' WHERE run_id = $1`, runID)
	require.NoError(t, err)
}

func (h *takeHarness) tracesOf(t *testing.T, runID, event string) []*state.Trace {
	t.Helper()
	traces, err := h.store.ListTraces(context.Background(), state.TraceFilter{RunID: runID, Event: event})
	require.NoError(t, err)
	return traces
}

// TestTakeoverE2E_CrashedExecutorConvergesToInterrupted is the §4-1
// headline: node-b starts an apply whose first step blocks in the
// channel (executor mid-flight), the lease is left to die (simulated
// crash), node-a's takeover settles the run to interrupted with a full
// audit chain, and the release of the blocked channel (the zombie
// waking) cannot pollute anything: no further step rows, no undo
// dispatch, no status overwrite.
func TestTakeoverE2E_CrashedExecutorConvergesToInterrupted(t *testing.T) {
	h := newTakeHarness(t)
	ctx := context.Background()
	const runID = "run-e2e-1"

	h.seedTakeRun(t, runID, "running")

	done := make(chan error, 1)
	go func() {
		_, _, _, err := h.b.adapter.Run(ctx, runID, false, 1)
		done <- err
	}()

	// The executor is mid-flight inside the blocked first step.
	select {
	case <-h.rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("executor never reached the blocked step")
	}

	// Simulated crash: the lease dies (a real kill -9 leaves exactly this
	// row state behind — no renewal). The takeover on node-a settles it.
	h.expireTakeLease(t, runID)
	res, err := h.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)
	assert.Contains(t, res.Settled, runID)

	run, err := h.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status, "§4-1: crashed executor converges to interrupted")

	// Audit chain integrity through the takeover.
	builder, err := audit.NewHashChainBuilder(h.store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, runID)
	require.NoError(t, err)
	verify := leveegrpc.NewAuditService(h.store)
	vr, err := verify.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: runID})
	require.NoError(t, err)
	assert.True(t, vr.GetValid(), "§4-1: audit hash chain stays valid across the takeover")

	traces := h.tracesOf(t, runID, "interrupted_by_takeover")
	assert.Len(t, traces, 1, "the takeover must be audited exactly once")

	// The zombie wakes (release the gate). Its remaining writes must all
	// fail closed: the evidence gate refuses persistence and the sentinel
	// surfaces — with NO undo dispatch (skip-rollback).
	close(h.rec.release)
	zombieErr := <-done
	require.Error(t, zombieErr)

	steps, err := h.store.ListSteps(ctx, state.StepFilter{RunID: runID, Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, steps, "§4-2: the zombie's evidence must be rejected (no step rows)")

	for _, c := range h.rec.snapshotCmds() {
		assert.NotEqual(t, "web-1\x00undo-command", c,
			"§4-2: a fenced-out zombie must never dispatch rollback undo commands")
		assert.NotEqual(t, "web-1\x00more-command", c,
			"§4-2: the zombie must not advance past the fence")
	}

	// Status untouched by the zombie's verdict.
	run, err = h.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status)
}

// TestTakeoverE2E_RetryInterruptedReDrives pins the §7.5-Q1 closed
// loop end to end: the executor blocks mid-flight, the takeover settles
// the run, the zombie is released (fenced out), and a RetryChange from
// node-a re-drives the change from its clean stored plan through the
// full fencing/evidence path to completion.
func TestTakeoverE2E_RetryInterruptedReDrives(t *testing.T) {
	h := newTakeHarness(t)
	ctx := context.Background()
	const runID = "run-e2e-2"

	h.seedTakeRun(t, runID, "running")

	done := make(chan error, 1)
	go func() {
		_, _, _, err := h.b.adapter.Run(ctx, runID, false, 1)
		done <- err
	}()
	select {
	case <-h.rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("executor never reached the blocked step")
	}

	// Crash + takeover.
	h.expireTakeLease(t, runID)
	_, err := h.loopy.TakeoverOnce(ctx)
	require.NoError(t, err)

	// The zombie wakes and is fenced out (drain it before retrying so the
	// run's in-process retry slot is free and the channel is reusable).
	close(h.rec.release)
	<-done

	// The blocking gate is single-use (a closed channel stays closed);
	// arm a fresh one for the retry so the re-drive runs to completion.
	h.rec.release = make(chan struct{})
	h.rec.entered = make(chan string, 16)
	close(h.rec.release) // unblocked for the retry

	require.NoError(t, h.a.adapter.Retry(ctx, runID, false, []string{"web-1"}))

	run, err := h.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status,
		"§7.5-Q1: an interrupted change must re-drive to completion through RetryChange")

	steps, err := h.store.ListSteps(ctx, state.StepFilter{RunID: runID, Limit: 100})
	require.NoError(t, err)
	assert.NotEmpty(t, steps, "the re-drive must persist its evidence")
	for _, s := range steps {
		assert.Equal(t, "success", s.Status)
	}

	// The retry finished with a fresh execution row gone (End released).
	lease, err := h.guard.GetExecution(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, lease)
}

// TestTakeoverE2E_NoDoubleWriteUnderConcurrentTakeovers pins §4-3:
// two takeover sweeps racing on the same expired run (from node-a and
// node-b) produce exactly one settlement — the per-run takeover lock and
// the status CAS serialise them.
func TestTakeoverE2E_NoDoubleWriteUnderConcurrentTakeovers(t *testing.T) {
	h := newTakeHarness(t)
	ctx := context.Background()
	const runID = "run-e2e-3"

	h.seedTakeRun(t, runID, "running")
	// An execution row owned by a (now dead) node-b executor.
	_, err := h.guard.Register(ctx, runID, "node-b", 10*time.Second)
	require.NoError(t, err)
	h.expireTakeLease(t, runID)

	// Both nodes sweep concurrently.
	var wg sync.WaitGroup
	settled := make(chan string, 2)
	for _, loopy := range []*takeover.Loop{h.loopy, h.loopyB} {
		wg.Add(1)
		go func(l *takeover.Loop) {
			defer wg.Done()
			res, err := l.TakeoverOnce(ctx)
			if err == nil && len(res.Settled) > 0 {
				settled <- res.Settled[0]
			}
		}(loopy)
	}
	wg.Wait()
	close(settled)

	winners := 0
	for range settled {
		winners++
	}
	assert.Equal(t, 1, winners, "§4-3: exactly one takeover may settle a run")

	traces := h.tracesOf(t, runID, "interrupted_by_takeover")
	assert.Len(t, traces, 1, "§4-3: no duplicate takeover traces")

	run, err := h.store.GetRun(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status)
}
