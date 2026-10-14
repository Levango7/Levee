package grpc

// rest_getchange_include_plan_test.go — `GET /changes/:id?includePlan=true` 的 REST 级兑现。
//
// 历史：这条曾经是"includePlan 惰性"的特征测试（#26 那批），故意断言"带与不带该参数响应逐字节
// 相同"，并在红字里点名要求实现者同步改 proto 注释与 docs/levee-api.md。2026-10-14（台账 #18 的
// plan 那半）兑现了该参数：`message Change` 加 `Plan plan = 13`、`GetChange` 从 `run.plan_json`
// 反序列化并经 `grpc.PlanToPB` 回填。红字如期触发 ⇒ 本文件按新契约改写为正面断言，docs 一节
// 也同步改写；服务层更细的四条子测试与三条变异见 `change_service_getchange_plan_test.go`。
//
// 断言用字面量而不是常量（[[feedback-gate-integrity]] 的同一条教训：写入端与断言端引用同一个
// 东西时，断言就是 `x == x`）。REST 网关的 protojson 用 **camelCase**（`changeId` /
// `workflowFile` / `impactSummary`），plan 块的键是 `plan`。本层只证"查询旗标 → 响应加宽"这条
// 端到端贯通；plan 内容逐项保真（changeId/batches/targetHosts 对齐 store 里的 plan.Plan）由服务层
// `change_service_getchange_plan_test.go` 用真实 `testPlan()` 序列化来钉。

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestRESTGetChange_IncludePlan(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	ctx := context.Background()

	now := time.Now().UTC()
	// 一个真带持久化 plan 的 run；不带 plan 的 run 只能证明"nil 是 nil"，证不出旗标是否生效。
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID:           "run-plan-1",
		WorkflowName: "name: plan\n",
		Status:       "planned",
		Creator:      "test",
		PlanJSON:     `{"change_id":"run-plan-1","target_hosts":["web-1"],"batches":[{"index":0,"hosts":["web-1"]}]}`,
		CreatedAt:    now,
		UpdatedAt:    now,
	}))

	// 另一条 run：无 plan_json。用来钉"includePlan=true 但没有 plan ⇒ 也不回显"的降级姿态。
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-plan-2", WorkflowName: "name: x\n", Status: "draft",
		Creator: "test", CreatedAt: now, UpdatedAt: now,
	}))

	get := func(id, query string) string {
		t.Helper()
		resp := doReq(t, http.MethodGet, srv.URL+"/changes/"+id+query, "")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode, "id=%s query=%q", id, query)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	// --- 有存储 plan 的 run：旗标必须真的加宽响应 ---
	bare := get("run-plan-1", "")
	asked := get("run-plan-1", "?includePlan=true")

	assert.NotEqual(t, bare, asked,
		"includePlan=true MUST change the response now that the flag is implemented")
	assert.NotContains(t, bare, `"plan"`,
		"default (includePlan unset) MUST NOT leak the plan — this is what keeps the wire snapshot stable")
	assert.Contains(t, asked, `"plan"`,
		"includePlan=true on a run with a stored plan must return a plan block")

	// --- 无存储 plan 的 run：include_plan=true 也降级为"没有 plan"，不造假 ---
	emptyAsked := get("run-plan-2", "?includePlan=true")
	assert.NotContains(t, emptyAsked, `"plan"`,
		"a run with no stored plan must degrade to no plan key, not an invented empty shape")
}
