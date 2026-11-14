package grpc

// rest_getchange_include_ticket_test.go — `GET /changes/:id?includeTicket=true`
// 的 REST 级兑现，与 includePlan 完全同形（#18 的 plan 那半立的先例）。
//
// 本层只证「查询旗标 → 响应加宽」这条端到端贯通，加上 protojson 的键名形状
// （camelCase：`hasSql`/`warnCount`/`persistence`）。逐项保真由服务层
// `change_service_getchange_ticket_test.go` 用真实 plan.Plan 序列化来钉。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

func TestRESTGetChange_IncludeTicket(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	ctx := context.Background()

	now := time.Now().UTC()
	// 一个真带 mysql.query 步骤的 run：只有这种 run 才证得出票是真的合成了，
	// 而不是一个恒空的壳。计划用结构体序列化，不手写 JSON——手写括号数错一
	// 次就会静默退化成「没有 SQL」，而那正是本测试要防的。
	p := &plan.Plan{
		ID: "run-ticket-1",
		Batches: []plan.Batch{{
			Index: 0, Targets: []string{"db-1"},
			Steps: []plan.PlanStep{{
				Name: "add-column", Module: "mysql", Action: "query",
				Args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN c INT"},
			}},
		}},
		TotalTargets: 1,
		SQLReview: &plan.SQLReviewSummary{
			Verdict: "warn", WarnCount: 1,
			Findings: []plan.SQLReviewFinding{{
				Step: "add-column", Rule: "SR-05", Severity: "warn", Statement: 1,
				Message: "ADD COLUMN without a default rewrites the table",
			}},
		},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID:           "run-ticket-1",
		WorkflowName: "db-change.yaml",
		Status:       "approved",
		Priority:     "high",
		IncidentID:   "prod",
		Creator:      "test",
		PlanJSON:     string(raw),
		CreatedAt:    now,
		UpdatedAt:    now,
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

	bare := get("run-ticket-1", "")
	asked := get("run-ticket-1", "?includeTicket=true")

	assert.NotEqual(t, bare, asked,
		"includeTicket=true MUST change the response now that the flag is implemented")
	assert.NotContains(t, bare, `"ticket"`,
		"default (includeTicket unset) MUST NOT leak the ticket — this keeps the wire snapshot stable")
	assert.Contains(t, asked, `"ticket"`,
		"includeTicket=true must return a ticket block")

	// protojson camelCase 键名与合成内容一并核对，避免只证"多了一个键"。
	assert.Contains(t, asked, `"hasSql":true`)
	assert.Contains(t, asked, `"SR-05"`)
	// The window's honest answer must survive serialisation: a bool false
	// would be omitted by protojson, so the field is a string that is always
	// present. This assertion fails if someone changes it back to a bool.
	assert.Contains(t, asked, `"persistence":"workflow-declaration"`)

	// An approval-less change renders an empty object (all fields are zero and
	// therefore omitted) — no fabricated hasApproval flag.
	assert.Contains(t, asked, `"approval":{}`)

	// No trace rows yet: the audit section says it could not verify rather
	// than reporting a passing chain.
	assert.Contains(t, asked, `"unverifiable":true`)
}
