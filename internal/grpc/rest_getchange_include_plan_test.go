package grpc

// rest_getchange_include_plan_test.go — `GET /changes/:id?includePlan=true` 的现状特征测试。
//
// 为什么要钉一条"什么都没发生"的测试：`proto/levee.proto` 的 GetChangeRequest 写着
// `// If true, include the latest plan and run summary in the response`，而实测这条承诺
// 没有任何实现——`internal/grpc/rest.go:753` 会把查询参数转成 `GetChangeRequest.IncludePlan`
// （路由是活的），但 `internal/grpc/change_service.go` 的 `GetChange` 直接
// `return runToPB(run), nil`，全仓 `GetIncludePlan()` 零命中；响应类型 `message Change`
// 只有 9 个字段，既没有 plan 也没有 run summary，所以就算读了这个 flag 也没有地方放。
// docs/levee-api.md 从未提到这个参数 ⇒ 承诺只活在注释里。
//
// 这条测试不是在庆祝现状，是在让现状**变响**：谁真把 include_plan 做出来，这里就会变红，
// 而红字的提示语要求他同时改 proto 注释与 docs——上一批同类缺陷（批次状态词表、chart 默认
// 镜像 tag）的成因都是"文档/注释与代码各说各话，而两边各自都有绿的测试"。
//
// 断言用字面量而不是常量（[[feedback-gate-integrity]] 的同一条教训：写入端与断言端引用
// 同一个东西时，断言就是 `x == x`）。

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

func TestRESTGetChange_IncludePlanIsCurrentlyInert(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	ctx := context.Background()

	now := time.Now().UTC()
	// A run that really does have a persisted plan: asserting "no plan comes back"
	// is only meaningful when there is one to withhold.
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID:           "run-plan-1",
		WorkflowName: "name: plan\n",
		Status:       "planned",
		Creator:      "test",
		PlanJSON:     `{"change_id":"run-plan-1","target_hosts":["web-1"],"batches":[{"index":0,"hosts":["web-1"]}]}`,
		CreatedAt:    now,
		UpdatedAt:    now,
	}))

	get := func(query string) string {
		t.Helper()
		resp := doReq(t, http.MethodGet, srv.URL+"/changes/run-plan-1"+query, "")
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode, "query=%q", query)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return string(body)
	}

	bare := get("")
	asked := get("?includePlan=true")

	assert.Equal(t, bare, asked,
		"includePlan=true currently changes nothing (change_service.GetChange never reads the flag). "+
			"If this goes red, the flag got implemented: update the GetChangeRequest comment in proto/levee.proto "+
			"AND docs/levee-api.md, which today says nothing about it.")

	// The plan is on its way out of the response even when asked — the payload shape
	// is pinned so a future implementation cannot just smuggle a blob in without
	// deciding which of the two representations it is (raw PlanJSON vs pb.Plan).
	assert.NotContains(t, asked, "target_hosts", "the plan's host list must not appear until include_plan is really implemented")
	assert.NotContains(t, asked, "plan_json", "GetChange returns Change (9 fields), not the stored plan blob")
}
