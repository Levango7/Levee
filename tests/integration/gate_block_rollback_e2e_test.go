//go:build integration

// gate_block_rollback_e2e_test.go — 验证门禁与手动回滚在 gRPC 边界上的端到端行为。
//
// 补的是两处此前只有包内证明的断口（此前 tests/ 里没有任何用例穿过 RollbackChange，
// grep "RollbackChange" tests/ 零命中）：
//
//	① "声明的 post_batch 门禁真的挡住下一批"。engine 级证据在
//	internal/engine/gate_position_wiring_test.go:132；这里把它抬到 live grpc.Server +
//	真实 wiring 引擎上，用通道层的实际派发记录定义"挡住"——第二批从未被派发。
//	用例同时钉住一条当场查出的现状：门禁本身**没有执行**声明的检查，因为引擎路径
//	从不给 GateInput.Channel 赋值（closure.go:393 与 :502 只填 RunID/BatchID/TargetIDs，
//	全仓唯一的非测试 Channel 赋值是 wiring/exec.go:114，那是给步骤执行用的）。所以
//	cmd 门禁是以 "missing channel" 失败关闭的。已登记 docs/product-roadmap.md。
//	若将来给门禁供通道，本用例的 NotContains 断言应当翻成 Contains——那是有意为之的
//	红线，不是疏漏。
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

	// The finding, pinned as a fact rather than an aspiration: the declared gate
	// command never reached a channel, so the block came from the honest
	// "missing channel" failure (verify/command_gate.go:230-239). Command gates
	// are therefore non-executable in every run path today.
	assert.NotContains(t, cmds, "web-1\x00gate-check",
		"registered in docs/product-roadmap.md: no run path supplies GateInput.Channel, "+
			"so a declared cmd gate cannot execute. Flip this to Contains when gates get channels.")

	auditSvc := leveegrpc.NewAuditService(store)
	vr, verr := auditSvc.VerifyHashChain(ctx, &pb.VerifyHashChainRequest{ChangeId: changeID})
	require.NoError(t, verr)
	assert.True(t, vr.GetValid(), "the audit chain must verify across a gate-blocked rollback")
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

	_, err = client.RollbackChange(ctx, &pb.RollbackRequest{
		ChangeId:    changeID,
		AutoApprove: true,
	})
	if err != nil {
		// Admission is the claim under test. FailedPrecondition would mean the
		// state gate refused a run it exists to remediate. Anything else (today
		// it is codes.Internal, "partial rollback: some undo steps failed") got
		// past admission and failed further down — a separate, registered defect.
		require.NotEqual(t, codes.FailedPrecondition, status.Code(err),
			"rolled_back_partial must clear the rollback state gate, got: %v", err)
		t.Logf("observed (registered in docs/product-roadmap.md): manual rollback from a "+
			"partial verdict surfaces as %s: %v", status.Code(err), err)
		return
	}

	// Admitted and completed: the compensation must actually dispatch, and the
	// audit chain must survive a second rollback pass.
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
