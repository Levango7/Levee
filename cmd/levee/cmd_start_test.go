package main

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

func TestStartCmdRegistrationAndLocalRefusal(t *testing.T) {
	cmd := findSub("start")
	require.NotNil(t, cmd)
	assert.Equal(t, "start", cmd.Name())
	for _, name := range []string{"force", "max-concurrency", "idempotency-key"} {
		assert.NotNil(t, cmd.Flags().Lookup(name), "start must register --%s", name)
	}
	assert.Contains(t, cmd.Flag("idempotency-key").Annotations, "cobra_annotation_bash_completion_one_required_flag")

	_, _, err := runSplit(t, "start", "run-local", "--idempotency-key", "local-request")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires --remote")
	assert.Contains(t, err.Error(), "apply --engine-enabled")
	assert.Equal(t, 2, exitCodeFor(err))
}

type cliStartEngine struct {
	calls       atomic.Int32
	started     chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func newCLIStartEngine() *cliStartEngine {
	return &cliStartEngine{started: make(chan struct{}), release: make(chan struct{})}
}

func (e *cliStartEngine) adapter() *grpc.EngineAdapter {
	return &grpc.EngineAdapter{Run: func(ctx context.Context, _ string, _ bool, _ int32) (string, bool, string, error) {
		e.calls.Add(1)
		e.startOnce.Do(func() { close(e.started) })
		select {
		case <-e.release:
			return "exec-cli-start", true, "", nil
		case <-ctx.Done():
			return "exec-cli-start", false, "", ctx.Err()
		}
	}}
}

func (e *cliStartEngine) finish() {
	e.releaseOnce.Do(func() { close(e.release) })
}

func createCLIStartableRun(t *testing.T, store state.Store) *state.Run {
	t.Helper()
	p := &plan.Plan{
		ID: "plan-cli-start", WorkflowName: "cli-start", TotalTargets: 1, CreatedAt: time.Now().UTC(),
		Batches: []plan.Batch{{
			Index: 0, Targets: []string{"web-1"}, MaxConcurrency: 1,
			Steps: []plan.PlanStep{{Name: "restart", Module: "svc", Action: "restart", Args: map[string]any{"name": "nginx"}}},
		}},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	now := time.Now().UTC()
	run := &state.Run{
		ID: "run-cli-start", Label: "cli-start", WorkflowName: "cli-start", Status: runstatus.StatusDraft,
		PlanJSON: string(raw), PlanHash: plan.ComputeHash(p), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.CreateRun(context.Background(), run))
	return run
}

func startCLIStartServer(t *testing.T, store state.Store, engine *grpc.EngineAdapter) string {
	t.Helper()
	changeSvc := grpc.NewChangeService(store, engine, nil, nil)
	srv := grpc.NewServer(store,
		grpc.WithListenAddr(":0"),
		grpc.WithChangeService(changeSvc),
		grpc.WithTemplateService(grpc.NewTemplateService(store, nil)),
		grpc.WithTargetService(grpc.NewTargetService(store, nil)),
		grpc.WithAuditService(grpc.NewAuditService(store)),
		grpc.WithSystemService(grpc.NewSystemService(store, &config.Config{}, "", "test", "test", "test", "go-test", time.Now())),
	)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(":0") }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if addr := srv.Addr(); addr != "" {
			t.Cleanup(func() { _ = srv.Stop() })
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("start test server did not bind")
	return ""
}

func TestStartCmdRemoteAcceptsAndReplays(t *testing.T) {
	store := newTestStore(t)
	run := createCLIStartableRun(t, store)
	engine := newCLIStartEngine()
	t.Cleanup(engine.finish)
	addr := startCLIStartServer(t, store, engine.adapter())
	args := []string{
		"--remote", "--server", addr, "--json", "start", run.ID,
		"--force", "--max-concurrency", "2", "--idempotency-key", "cli-start-request",
	}

	stdout, _, err := runSplit(t, args...)
	require.NoError(t, err)
	var accepted struct {
		Data struct {
			ExecutionID string `json:"execution_id"`
			Status      string `json:"status"`
			Duplicate   bool   `json:"duplicate"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &accepted), stdout)
	assert.Equal(t, run.ID, accepted.Data.ExecutionID)
	assert.Equal(t, runstatus.StatusRunning, accepted.Data.Status)
	assert.False(t, accepted.Data.Duplicate)
	select {
	case <-engine.started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted remote execution did not reach the engine")
	}

	stdout, _, err = runSplit(t, args...)
	require.NoError(t, err)
	var replay struct {
		Data struct {
			ExecutionID string `json:"execution_id"`
			Status      string `json:"status"`
			Duplicate   bool   `json:"duplicate"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &replay), stdout)
	assert.Equal(t, run.ID, replay.Data.ExecutionID)
	assert.Equal(t, runstatus.StatusRunning, replay.Data.Status)
	assert.True(t, replay.Data.Duplicate)
	assert.Equal(t, int32(1), engine.calls.Load())

	engine.finish()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.GetRun(context.Background(), run.ID)
		require.NoError(t, err)
		if current.Status == runstatus.StatusCompleted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := store.GetRun(context.Background(), run.ID)
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusCompleted, current.Status)
}
