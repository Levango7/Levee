//go:build integration

// engine_exec_test.go is the design-A acceptance at the gRPC surface: a
// REAL internal/wiring engine (loopback transport standing in for ssh) is
// served through a live grpc.Server over TCP, so the full
// plan → approve → apply cycle crosses real request boundaries. It proves
// the §4 acceptance bullets end-to-end: real steps settle the run to
// "completed" with batch/step evidence rows and a verifiable audit hash
// chain; a forced step failure auto-rolls-back and reaches "rolled_back"
// via serve; an unplanned (or plan-tampered) change is refused before any
// dispatch. The engine-level closure paths are covered by
// internal/wiring/exec_run_test.go; this file pins that the serve/gRPC
// layer wires and exposes them faithfully.

package integration

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	ggrpc "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/channel"
	leveegrpc "github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/wiring"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- loopback transport (mirrors internal/wiring/exec_run_test.go) ----------

type loopRecorder struct {
	mu      sync.Mutex
	dials   []string
	cmds    []string // "host\x00cmd"
	closes  int
	failCmd map[string]bool // exact command → force exit 1

	// release, when non-nil, makes Exec block until closed (the executor
	// "hangs" mid-step); entered signals that a channel is inside the
	// gate. Used by the takeover e2e suite for deterministic crash
	// staging.
	release chan struct{}
	entered chan string
}

func (r *loopRecorder) recordDial(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dials = append(r.dials, host)
}

func (r *loopRecorder) recordCmd(host, cmd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, host+"\x00"+cmd)
}

func (r *loopRecorder) recordClose() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
}

func (r *loopRecorder) snapshotCmds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

func (r *loopRecorder) snapshotDials() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dials...)
}

func (r *loopRecorder) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

type loopChannel struct {
	host string
	rec  *loopRecorder

	mu        sync.Mutex
	connected bool
}

func (c *loopChannel) Connect(context.Context) error {
	c.rec.recordDial(c.host)
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	return nil
}

func (c *loopChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	if c.rec.release != nil {
		if c.rec.entered != nil {
			select {
			case c.rec.entered <- c.host:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.rec.release:
		}
	}
	c.rec.recordCmd(c.host, cmd)
	if c.rec.failCmd[cmd] {
		return &channel.ExecResult{ExitCode: 1, Stderr: "loopback: forced failure for " + cmd}, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "loopback ok"}, nil
}

func (c *loopChannel) Upload(context.Context, string, io.Reader) error {
	return fmt.Errorf("loopback: Upload not supported")
}

func (c *loopChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, fmt.Errorf("loopback: Download not supported")
}

func (c *loopChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		c.connected = false
		c.rec.recordClose()
	}
	return nil
}

func (c *loopChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

type loopFactory struct{ rec *loopRecorder }

func (f loopFactory) Create(target channel.Target) (channel.Channel, error) {
	return &loopChannel{host: target.Host(), rec: f.rec}, nil
}

// --- serve harness -----------------------------------------------------------

// serveEngine starts a live grpc.Server whose ChangeService is backed by a
// real wiring.Engine (loopback channel registry, no credential resolver —
// the fake transport needs none). Mirrors `serve --engine-enabled` minus
// process bootstrap.
func serveEngine(t *testing.T, rec *loopRecorder, hosts ...string) (pb.ChangeServiceClient, state.Store) {
	t.Helper()
	ctx := context.Background()
	store := newTestStore(t)
	for _, h := range hosts {
		require.NoError(t, store.UpsertTarget(ctx, &state.Target{
			ID:          "tgt-" + h,
			Hostname:    h,
			Port:        22,
			ChannelType: "local",
			Status:      "active",
			CreatedAt:   time.Now().UTC(),
		}))
	}

	reg := channel.NewChannelRegistry()
	reg.Register("local", loopFactory{rec: rec})
	eng := wiring.NewEngine(store, wiring.WithChannelRegistry(reg))
	svc := leveegrpc.NewChangeService(store, eng.Adapter(), nil, nil)
	srv := leveegrpc.NewServer(store, leveegrpc.WithChangeService(svc))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start("127.0.0.1:0") }()
	require.Eventually(t, func() bool { return srv.Addr() != "" },
		5*time.Second, 20*time.Millisecond, "server never bound to an ephemeral port")
	select {
	case err := <-errCh:
		require.NoError(t, err, "server Start failed")
	case <-time.After(50 * time.Millisecond):
		// Start still serving (blocks in Serve) — the expected state.
	}
	t.Cleanup(func() { _ = srv.Stop() })

	conn, err := ggrpc.NewClient(srv.Addr(), ggrpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewChangeServiceClient(conn), store
}

const serveExecWorkflowYAML = `name: serve-exec
target:
  type: host
  query: "env=test"
steps:
  - name: noop
    action: shell.exec
    args:
      cmd: noop-command
`

const serveRBWorkflowYAML = `name: serve-rb
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
  - name: boom
    action: shell.exec
    args:
      cmd: fail-command
`

// planApproveChange drives the client through PlanChange and
// ApproveChange, asserting the run carries the hash-bound artifact the
// apply gate demands.
func planApproveChange(t *testing.T, client pb.ChangeServiceClient, store state.Store, changeID string, hosts []string) {
	t.Helper()
	ctx := context.Background()
	_, err := client.PlanChange(ctx, &pb.PlanChangeRequest{ChangeId: changeID, TargetHosts: hosts})
	require.NoError(t, err)
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	assert.NotEmpty(t, run.PlanJSON, "plan artifact must be persisted on the run")
	assert.NotEmpty(t, run.PlanHash)

	_, err = client.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: changeID,
		Comment:  "engine exec acceptance",
	})
	require.NoError(t, err)
}

func createChange(t *testing.T, client pb.ChangeServiceClient, label, workflowYAML string) string {
	t.Helper()
	resp, err := client.CreateChange(context.Background(), &pb.CreateChangeRequest{
		Label:        label,
		Priority:     "medium",
		WorkflowFile: workflowYAML,
		TemplateName: "engine-exec",
	})
	require.NoError(t, err)
	return resp.GetId()
}

// --- tests ---------------------------------------------------------------------

// TestEngineServe_CompletesWithEvidenceAndChain is the §4 headline: a
// gRPC client plans, approves and applies; real steps dispatch over the
// (loopback) channel; the run settles to completed with batch/step
// evidence rows and a valid audit hash chain.
func TestEngineServe_CompletesWithEvidenceAndChain(t *testing.T) {
	ctx := context.Background()
	rec := &loopRecorder{}
	client, store := serveEngine(t, rec, "web-1", "web-2")

	changeID := createChange(t, client, "serve-exec-e2e", serveExecWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1", "web-2"})

	applyResp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    false, // approved through the real approval gate
		MaxConcurrency: 1,
	})
	require.NoError(t, err)
	assert.True(t, applyResp.GetSuccess())
	assert.Equal(t, "completed", applyResp.GetChange().GetStatus())
	assert.NotEmpty(t, applyResp.GetRunId(), "apply must surface the engine run id")

	// Batch + step evidence rows persisted under the change id.
	batches, err := store.ListBatches(ctx, state.BatchFilter{RunID: changeID})
	require.NoError(t, err)
	require.Len(t, batches, 1)
	assert.Equal(t, "completed", batches[0].Status)
	assert.Equal(t, 2, batches[0].Succeeded)

	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: changeID, Limit: 200})
	require.NoError(t, err)
	require.Len(t, steps, 2)
	for _, s := range steps {
		assert.Equal(t, "success", s.Status)
		assert.Equal(t, "shell.exec", s.Action)
		require.NotNil(t, s.ExitCode)
		assert.Equal(t, 0, *s.ExitCode)
		assert.Equal(t, "loopback ok", s.Stdout)
	}

	// Every dialled channel was closed at run teardown.
	assert.Equal(t, rec.closeCount(), len(rec.snapshotDials()))

	// Audit chain: build and verify through the audit service surface.
	builder, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, changeID)
	require.NoError(t, err)
	auditSvc := leveegrpc.NewAuditService(store)
	verifyResp, err := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: changeID})
	require.NoError(t, err)
	assert.True(t, verifyResp.GetValid(), "audit hash chain must verify after real execution")
}

// TestEngineServe_AutoRollbackReachesRolledBack pins §4 "rollback reaches
// rolled_back via serve": a forced step failure auto-rolls-back inside the
// closure, and the gRPC ApplyResponse carries the distinguishing
// rolled_back status plus the undo evidence rows.
func TestEngineServe_AutoRollbackReachesRolledBack(t *testing.T) {
	ctx := context.Background()
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true}}
	client, store := serveEngine(t, rec, "web-1")

	changeID := createChange(t, client, "serve-rb-e2e", serveRBWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1"})

	applyResp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    false,
		MaxConcurrency: 1,
	})
	require.NoError(t, err, "a rolled-back apply is an outcome, not an RPC error")
	assert.False(t, applyResp.GetSuccess())
	assert.Equal(t, "rolled_back", applyResp.GetChange().GetStatus())

	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: changeID, Limit: 200})
	require.NoError(t, err)
	byName := map[string][]*state.Step{}
	for _, s := range steps {
		byName[s.StepName] = append(byName[s.StepName], s)
	}
	rowWith := func(name, status string) *state.Step {
		for _, s := range byName[name] {
			if s.Status == status {
				return s
			}
		}
		return nil
	}
	require.NotNil(t, rowWith("work", "success"), "forward step evidence must persist")
	boom := rowWith("boom", "failed")
	require.NotNil(t, boom, "failed forward step evidence must persist")
	assert.Contains(t, boom.Stderr, "forced failure")
	require.NotNil(t, rowWith("undo-work", "success"), "rollback undo evidence must persist via serve")
	// boom has no rollback spec: the manager records the undo as skipped.
	require.NotNil(t, rowWith("boom", "skipped"))

	// The undo command ran; the poisoned forward command was what failed.
	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00work-command")
	assert.Contains(t, cmds, "web-1\x00undo-command")
}

// TestEngineServe_RefusesUnplannedAndTamperedPlan pins §4 "drift refused
// by plan_hash" at the gRPC boundary: neither a never-planned run nor a
// run whose stored plan hash was altered may execute — both are refused
// with FailedPrecondition before a single dispatch, run status untouched.
func TestEngineServe_RefusesUnplannedAndTamperedPlan(t *testing.T) {
	ctx := context.Background()
	rec := &loopRecorder{}
	client, store := serveEngine(t, rec, "web-1")

	// Never planned: refused by the plan gate (draft + auto-approve passes
	// the state gate; approve itself already refuses unplanned runs),
	// status untouched, nothing dispatched.
	unplanned := createChange(t, client, "serve-noplan-e2e", serveExecWorkflowYAML)
	_, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:    unplanned,
		AutoApprove: true,
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "no persisted plan")
	run, err := store.GetRun(ctx, unplanned)
	require.NoError(t, err)
	assert.Equal(t, "draft", run.Status, "refused apply must not mutate status")

	// Planned, then the plan artifact is tampered after approval: the
	// hash gate refuses.
	tampered := createChange(t, client, "serve-tamper-e2e", serveExecWorkflowYAML)
	planApproveChange(t, client, store, tampered, []string{"web-1"})
	run, err = store.GetRun(ctx, tampered)
	require.NoError(t, err)
	run.PlanHash = fmt.Sprintf("%064x", 0xdeadbeef)
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = client.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: tampered})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	assert.Empty(t, rec.snapshotCmds(), "no dispatch may cross the wire after a refused apply")
}
