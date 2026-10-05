//go:build integration

// gate_block_rollback_e2e_test.go — 验证门禁与手动回滚在 gRPC 边界上的端到端行为。
//
// 补的是两处此前只有包内证明的断口（此前 tests/ 里没有任何用例穿过 RollbackChange，
// grep "RollbackChange" tests/ 零命中）：
//
//	① "声明的 post_batch 门禁真的挡住下一批"。engine 级证据在
//	internal/engine/gate_position_wiring_test.go:132；这里把它抬到 live grpc.Server +
//	真实 wiring 引擎上，用通道层的实际派发记录定义"挡住"——第二批从未被派发。
//	本文件同时钉住门禁的两侧：失败的门禁挡住下一批（TestEngineServe_BatchGateStops…），
//	通过的门禁让整条 rollout 走完（TestEngineServe_BatchGatePassing…）。后者在以前是
//	不可能成立的——引擎路径从不给 GateInput.Channel 赋值，cmd 门禁一律以 "missing
//	channel" 失败关闭，声明了命令门禁的变更永远过不去自己那一关。现在通道由本次执行
//	自己的会话缓存供给（wiring runExec.gateChannelProvider），门禁与步骤走同一条连接、
//	同一份租约、同一套凭据。
//
//	② RollbackChange 的准入：rolled_back_partial 必须跨过状态门（那正是手动回滚存在
//	的理由），干净的 rolled_back 必须被拒（双重 undo 保护）。

package integration

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	leveegrpc "github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/runstatus"
)

const serveBatchGateWorkflowYAML = `name: batch-gate-block
target:
  type: host
  query: "env=test"
batches:
  strategy: one-per-target
  gate:
    cmd:
      run: gate-check
      expect_exit: 0
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
`

// rollbackStatusFamily: the closure decides among these by whether every applied
// step had a compensation, so the assertion pins "it stopped and reversed" rather
// than one string the grader happens to pick.
var rollbackStatusFamily = []string{"rolled_back", "rolled_back_partial", "rollback_incomplete"}

func rollbackActorCtx() context.Context {
	return metadata.NewOutgoingContext(context.Background(),
		metadata.Pairs("x-actor", engineApprover))
}

func TestEngineServe_BatchGateStopsTheNextBatchOverGRPC(t *testing.T) {
	ctx := context.Background()
	rec := &loopRecorder{failCmd: map[string]bool{"gate-check": true}}
	client, store := serveEngine(t, rec, "web-1", "web-2")

	changeID := createChange(t, client, "batch-gate-e2e", serveBatchGateWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1", "web-2"})

	resp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    false,
		MaxConcurrency: 1,
	})
	require.NoError(t, err, "a gate-blocked rollback is an outcome, not an RPC error")
	assert.False(t, resp.GetSuccess(), "a blocked gate must not report a successful apply")

	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	assert.Contains(t, rollbackStatusFamily, run.Status,
		"the run must settle in the rollback family, got %q", run.Status)
	t.Logf("gate-blocked run settled as %q", run.Status)

	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00work-command", "batch 1 must have been applied")
	assert.NotContains(t, cmds, "web-2\x00work-command",
		"a failing post-batch gate must stop the rollout before batch 2, dispatches: %v", cmds)
	assert.Contains(t, cmds, "web-1\x00undo-command",
		"the applied batch must be reversed, dispatches: %v", cmds)

	// The check itself must reach the target. This is the assertion that used to
	// read "the declared gate never executes"; per-target fan-out now comes from
	// the run's own cached, lease-checked sessions.
	assert.Contains(t, cmds, "web-1\x00gate-check",
		"a declared cmd gate must execute on the target it guards, dispatches: %v", cmds)
	assert.NotContains(t, cmds, "web-2\x00gate-check",
		"the rollout stops at the failing batch, so batch 2 is applied neither checked nor run: %v", cmds)

	auditSvc := leveegrpc.NewAuditService(store)
	vr, verr := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: changeID})
	require.NoError(t, verr)
	assert.True(t, vr.GetValid(), "the audit chain must verify across a gate-blocked rollback")
}

// TestEngineServe_BatchGatePassingLetsEveryBatchRun is the half that could not
// exist before: a declared cmd gate that passes on every target must let the
// rollout finish. With no channel supplied the same workflow rolled itself back
// on its own gate, so "the gate can pass" was not merely unproven — it was
// unreachable. Both batches must be applied AND checked, in that order.
func TestEngineServe_BatchGatePassingLetsEveryBatchRun(t *testing.T) {
	ctx := context.Background()
	rec := &loopRecorder{}
	client, store := serveEngine(t, rec, "web-1", "web-2")

	changeID := createChange(t, client, "batch-gate-pass-e2e", serveBatchGateWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1", "web-2"})

	resp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    false,
		MaxConcurrency: 1,
	})
	require.NoError(t, err)
	assert.True(t, resp.GetSuccess(), "a passing batch gate must not block the rollout")

	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status, "the change must finish once every gate passes")

	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00work-command")
	assert.Contains(t, cmds, "web-2\x00work-command", "batch 2 must run once batch 1's gate passed")
	assert.Contains(t, cmds, "web-1\x00gate-check", "the gate runs after every batch, not only the last")
	assert.Contains(t, cmds, "web-2\x00gate-check")
	assert.NotContains(t, cmds, "web-1\x00undo-command", "nothing may be reversed when nothing failed")
}

func TestEngineServe_RollbackChangeClearsTheStateGateFromPartialVerdict(t *testing.T) {
	ctx := rollbackActorCtx()
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true}}
	client, store := serveEngine(t, rec, "web-1")

	changeID := createChange(t, client, "serve-rb-manual-e2e", serveRBWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1"})

	applyResp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       changeID,
		AutoApprove:    false,
		MaxConcurrency: 1,
	})
	require.NoError(t, err)
	assert.False(t, applyResp.GetSuccess())
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back_partial", run.Status,
		"premise: D-2 v2 reports the partial verdict that manual rollback exists to remediate")
	require.Contains(t, runstatus.RollbackAdmitted, run.Status,
		"the e2e premise must agree with the single-sourced admission set")

	before := len(rec.snapshotCmds())
	resp, err := client.RollbackChange(ctx, &pb.RollbackRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	// The registered defect this line used to tolerate: an unfinished manual
	// rollback came back as codes.Internal and left the run on its pre-attempt
	// status. It is now an outcome — Success=false with a status that says so —
	// and the attempt stays inside the retryable set.
	require.NoError(t, err, "an unfinished manual rollback must be reported as an outcome, not a server fault")
	assert.False(t, resp.GetSuccess(), "the response must not claim a completed rollback")
	assert.Contains(t, resp.GetMessage(), "incomplete",
		"the message must say the attempt did not finish, got %q", resp.GetMessage())
	t.Logf("attempt dispatched %d channel calls (before=%d), hosts=%v",
		len(rec.snapshotCmds()), before, resp.GetRolledBackHosts())

	run, err = store.GetRun(ctx, changeID)
	require.NoError(t, err)
	assert.Equal(t, runstatus.StatusRollbackIncomplete, run.Status,
		"the record must advance from the pre-attempt verdict to what actually happened")
	assert.Contains(t, runstatus.RollbackAdmitted, run.Status,
		"an unfinished rollback stays retryable, never sealed")

	auditSvc := leveegrpc.NewAuditService(store)
	vr, verr := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: changeID})
	require.NoError(t, verr)
	assert.True(t, vr.GetValid(), "manual rollback must not break the audit chain")
}

// TestEngineServe_RollbackChangeRefusedAfterCleanRollback pins the exclusion
// half of the admission set at the wire boundary: a change whose rollback
// completed cleanly must not be rolled back again (double-undo protection), and
// the refusal must land before anything is dispatched.
func TestEngineServe_RollbackChangeRefusedAfterCleanRollback(t *testing.T) {
	ctx := rollbackActorCtx()
	rec := &loopRecorder{failCmd: map[string]bool{"gate-check": true}}
	client, store := serveEngine(t, rec, "web-1", "web-2")

	changeID := createChange(t, client, "batch-gate-refuse-e2e", serveBatchGateWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1", "web-2"})
	_, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: changeID, MaxConcurrency: 1})
	require.NoError(t, err)

	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.Equal(t, "rolled_back", run.Status,
		"premise: every applied step had a compensation, so the verdict is clean, got %q", run.Status)

	before := len(rec.snapshotCmds())
	_, err = client.RollbackChange(ctx, &pb.RollbackRequest{ChangeId: changeID, AutoApprove: true})
	require.Error(t, err, "a clean rolled_back change must not be re-rolled back")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Len(t, rec.snapshotCmds(), before, "the refusal must land before any dispatch")
}

// TestEngineServe_RollbackChangeFromCompletedClearsStateGate records a defect
// rather than endorsing it: `completed` is admitted (an operator may roll back a
// success), but when the change's steps carry no compensation the RPC answers
// codes.Internal — "engine rollback: partial rollback: some undo steps failed".
// A domain outcome (nothing left to undo) reaching the client as a server
// programming error leaves it no way to decide whether to retry, and it is the
// same mapping seen from rolled_back_partial. Registered in
// docs/product-roadmap.md; this test pins only the admission half, so it stays
// green when the mapping is fixed and goes red if the state gate ever regresses.
func TestEngineServe_RollbackChangeFromCompletedClearsStateGate(t *testing.T) {
	ctx := rollbackActorCtx()
	rec := &loopRecorder{}
	client, store := serveEngine(t, rec, "web-1")

	changeID := createChange(t, client, "serve-exec-completed-e2e", serveExecWorkflowYAML)
	planApproveChange(t, client, store, changeID, []string{"web-1"})
	applyResp, err := client.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: changeID})
	require.NoError(t, err)
	require.True(t, applyResp.GetSuccess())
	run, err := store.GetRun(ctx, changeID)
	require.NoError(t, err)
	require.Equal(t, "completed", run.Status)
	require.Contains(t, runstatus.RollbackAdmitted, run.Status,
		"the premise must agree with the single-sourced admission set")

	_, err = client.RollbackChange(ctx, &pb.RollbackRequest{ChangeId: changeID, AutoApprove: true})
	if err != nil {
		require.NotEqual(t, codes.FailedPrecondition, status.Code(err),
			"completed is admitted by the state gate; a refusal here means the admission set "+
				"and the guard diverged: %v", err)
		t.Logf("observed (registered in docs/product-roadmap.md): rollback of a completed "+
			"change without compensations surfaces as %s: %v", status.Code(err), err)
		return
	}
	// If it ever succeeds, the audit chain must still verify.
	auditSvc := leveegrpc.NewAuditService(store)
	vr, verr := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: changeID})
	require.NoError(t, verr)
	assert.True(t, vr.GetValid())
}
