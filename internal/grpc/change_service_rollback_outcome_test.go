package grpc

// change_service_rollback_outcome_test.go — RollbackChange 的两种"不成功"必须分开。
//
// "跑了但没补全"是领域结局：补偿已经派发到目标机，操作者需要知道结果并重试，所以
// 它是 Success=false + rollback_incomplete；而 codes.Internal 会把 run 记录留在尝试
// 之前的状态，仿佛什么都没发生——那正是本轮审计查出的行为（engine.Rollback 返回任何
// error 都被一律 Internal 吞掉，且从不更新状态）。真正的服务端故障仍必须是 Internal
// 且不改写历史，两条一起钉住：任何一边退化成另一边都会被发现。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

func TestRollbackChangeReportsIncompleteAsOutcome(t *testing.T) {
	// Own adapter, not recordingEngine: the shared fake drops the rollback run id
	// and the host list whenever it returns an error, while wiring (run.go:645)
	// hands both back *alongside* the outcome. Asserting against the stub's habit
	// would pin a shape production never produces.
	svc, store := newTestChangeServiceWithEngine(t, &EngineAdapter{
		Rollback: func(_ context.Context, _, _ string, _ bool) (string, []string, error) {
			return "rb-partial", []string{"web-1"},
				fmt.Errorf("%w: %w", rollback.ErrIncomplete,
					fmt.Errorf("partial rollback: some undo steps failed"))
		},
	})
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-outcome"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: created.GetId()})
	require.NoError(t, err, "an incomplete rollback is an outcome, not a server fault")
	assert.False(t, resp.GetSuccess(), "the response must not claim success")
	assert.Equal(t, "rb-partial", resp.GetRollbackRunId(),
		"the attempt happened, so it must stay addressable in the evidence")
	assert.Equal(t, runstatus.StatusRollbackIncomplete, resp.GetChange().GetStatus())
	assert.Equal(t, []string{"web-1"}, resp.GetRolledBackHosts(),
		"what did get restored stays reportable even when the whole did not finish")
	assert.Contains(t, resp.GetMessage(), "some undo steps failed",
		"the message must name what did not restore, not just that something failed")

	ctx := context.Background()
	run, gerr := store.GetRun(ctx, created.GetId())
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRollbackIncomplete, run.Status,
		"the record must say a rollback was attempted and did not finish")
	assert.Contains(t, runstatus.RollbackAdmitted, run.Status,
		"an unfinished rollback must stay retryable, not be sealed into a terminal state")

	require.Eventually(t, func() bool {
		entries, aerr := store.ListAudits(ctx, state.AuditFilter{RunID: created.GetId()})
		if aerr != nil {
			return false
		}
		for _, a := range entries {
			if a.Action == "rollback" && a.Result == runstatus.StatusRollbackIncomplete {
				return true
			}
		}
		return false
	}, 3*time.Second, 50*time.Millisecond, "the audit trail must record the outcome, not only the attempt")
}

func TestRollbackChangeStillReportsServerFaultAsInternal(t *testing.T) {
	engine := &recordingEngine{rbErr: fmt.Errorf("credential store unreachable")}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-fault"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	_, err = svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: created.GetId()})
	require.Error(t, err, "a genuine server fault must not be dressed up as an outcome")
	assert.Equal(t, codes.Internal, status.Code(err))

	run, gerr := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBackPartial, run.Status,
		"nothing ran to completion, so the record must not be rewritten")
}
