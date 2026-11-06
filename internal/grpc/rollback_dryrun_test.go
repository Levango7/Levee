package grpc

// rollback_dryrun_test.go — RollbackChange 的 dry_run 必须是只读的。
//
// 修复前的行为：服务层根本不读 req.GetDryRun()，于是「预览」请求得到的是一次
// 真的回滚——补偿派发到生产主机，run 状态被改写，审计链留下一条 rollback 记录。
// 控制台的补救向导随后把对话框里的「待补偿」列成刚刚已经执行掉的主机，而真正
// 的「现在补」那一次又因为状态已经变成 rolled_back 而被准入检查拒绝。
//
// 这一层钉的是服务层的三件事：flag 必须传到引擎、预览必须原样回报两个主机
// 列表、预览必须不留下任何痕迹（run / 审计 / 事件）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"
)

// TestRollbackChangeDryRunDoesNotExecuteOrPersist pins the whole point: a
// preview reports and changes nothing.
func TestRollbackChangeDryRunDoesNotExecuteOrPersist(t *testing.T) {
	engine := &recordingEngine{
		rollbackID: "rb-preview",
		rbHosts:    []string{"web-1"},
		rbSkipped:  []string{"web-2"},
	}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-preview"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{
		ChangeId: created.GetId(),
		DryRun:   true,
	})
	require.NoError(t, err)

	// The flag must reach the engine: a service that answered the preview
	// itself would have to duplicate the engine's host accounting.
	assert.True(t, engine.lastDryRun, "the dry_run flag must reach the engine")
	assert.Equal(t, int32(1), atomic.LoadInt32(&engine.rollbackCalled))

	// The preview reports what would happen...
	assert.Equal(t, []string{"web-1"}, resp.GetRolledBackHosts(),
		"the hosts a commit would compensate must be reported")
	assert.Equal(t, []string{"web-2"}, resp.GetSkippedHosts(),
		"the hosts a commit would leave alone must be reported too")
	assert.Empty(t, resp.GetRollbackRunId(), "a preview mints no rollback run")

	// ...and changes nothing.
	run, gerr := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBackPartial, run.Status,
		"a preview must not rewrite the run status")

	audits, aerr := store.ListAudits(context.Background(), state.AuditFilter{RunID: created.GetId()})
	require.NoError(t, aerr)
	for _, a := range audits {
		assert.NotEqual(t, state.AuditActionRollback, a.Action,
			"a preview must not leave a rollback row in the audit chain")
	}
}

// TestRollbackChangeCommitIsNotADryRun is the other half of the pair: the
// ordinary call must still execute, and must still leave the evidence a
// rollback is supposed to leave.
func TestRollbackChangeCommitIsNotADryRun(t *testing.T) {
	engine := &recordingEngine{
		rollbackID: "rb-real",
		rbHosts:    []string{"web-1"},
	}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-real"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{ChangeId: created.GetId()})
	require.NoError(t, err)

	assert.False(t, engine.lastDryRun, "an ordinary rollback is not a preview")
	assert.Equal(t, "rb-real", resp.GetRollbackRunId())
	assert.Equal(t, []string{"web-1"}, resp.GetRolledBackHosts())

	run, gerr := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBack, run.Status,
		"a real rollback rewrites the status — that is the whole point of it")
}

// TestRollbackChangeDryRunWithoutEngineDoesNotMarkTheRun covers the
// status-only deployment: there is no plan to walk and no host to
// compensate, so the preview can only say so. It must NOT fall through to
// the "mark it rolled_back" fallback — that would make the preview the very
// mutation it was asked to precede.
func TestRollbackChangeDryRunWithoutEngineDoesNotMarkTheRun(t *testing.T) {
	svc, store := newTestChangeServiceWithEngine(t, nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-noeng"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	resp, err := svc.RollbackChange(context.Background(), &pb.RollbackRequest{
		ChangeId: created.GetId(),
		DryRun:   true,
	})
	require.NoError(t, err)
	assert.Empty(t, resp.GetRolledBackHosts(), "no engine means no host would be compensated")
	assert.Contains(t, resp.GetMessage(), "no execution engine",
		"the operator must be told why the preview is empty")

	run, gerr := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, gerr)
	assert.Equal(t, runstatus.StatusRolledBackPartial, run.Status,
		"a preview must not mark the run rolled_back")
}

// TestApplyChangeRefusesDryRunInsteadOfApplying closes the second hole of
// the same class: ApplyChange's proto says dry_run means "only print the
// plan and exit", but the flag was ignored — so a caller asking for a
// preview got an applied change. The plan-printing path is PlanChange
// (whose dry_run IS honoured), so ApplyChange refuses and says where to go.
func TestApplyChangeRefusesDryRunInsteadOfApplying(t *testing.T) {
	engine := &recordingEngine{runID: "run-1", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "apply-preview"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "approved")

	_, err = svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{
		ChangeId: created.GetId(),
		DryRun:   true,
	})
	require.Error(t, err, "a preview request must not apply the change")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "PlanChange",
		"the refusal must name the endpoint that does answer the question")
	assert.Equal(t, int32(0), atomic.LoadInt32(&engine.runCalled),
		"nothing may be dispatched for a refused preview")
}

// TestRollbackPreviewCarriesTheHostListsOnTheWire pins the WIRE contract of
// the remediation wizard — the one the console's evidence panel depends on.
//
// The console reads `rolledBackHosts` and `skippedHosts` out of the rollback
// response. Two things have to be true for that to work, and neither was
// pinned anywhere:
//
//   - the SPELLING: protojson emits lowerCamelCase, so the proto's
//     `rolled_back_hosts` reaches the browser as `rolledBackHosts`;
//   - the PRESENCE: `skippedHosts` was a key the server NEVER sent at all,
//     because the service never assigned the field. A client that reads a
//     key the wire does not carry gets `undefined`; every consumer here
//     defaults it, so the panel renders blank and NOTHING errors.
//
// The committed wire-contract snapshot (web/src/api/__contract__/wire.json)
// covers the GET endpoints, and the rollback response is a POST — so this is
// the one place the two host lists are actually checked end to end.
func TestRollbackPreviewCarriesTheHostListsOnTheWire(t *testing.T) {
	engine := &recordingEngine{
		rollbackID: "rb-wire",
		rbHosts:    []string{"10.0.1.11"},
		rbSkipped:  []string{"10.0.1.12"},
	}
	store := newTestStore(t)
	change := NewChangeService(store, engine.adapter(), nil, nil)
	gw := NewGateway(ServeGatewayConfig{Addr: "127.0.0.1:0"})
	gw.SetServices(change, nil, nil, nil, nil, nil, nil, nil)

	created, err := change.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "rb-wire"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), runstatus.StatusRolledBackPartial)

	req := httptest.NewRequest(http.MethodPost,
		"/changes/"+created.GetId()+"/rollback", strings.NewReader(`{"dryRun":true}`))
	rec := httptest.NewRecorder()
	gw.restRoute().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	assert.Equal(t, []any{"10.0.1.11"}, body["rolledBackHosts"],
		"the hosts a commit would compensate must reach the console under the name it reads")
	assert.Equal(t, []any{"10.0.1.12"}, body["skippedHosts"],
		"the hosts a commit would leave alone must reach the console too — this key was never sent before")
	assert.NotContains(t, body, "rolled_back_hosts",
		"the console reads camelCase; a snake_case twin would read back undefined")
	assert.NotContains(t, body, "skipped_hosts")
	assert.NotContains(t, body, "rollbackRunId",
		"a preview mints no rollback run, so the console must not see one")
	assert.Equal(t, true, body["success"])
}
