package grpc

// rest_ticket_export_test.go — 发布单的 REST 导出面。
//
// 两条必须成立的事实：
//
//  1. `?format=markdown` 返回的是**文档**，不是「JSON 里塞一段 Markdown」
//     ——内容类型是 text/markdown，浏览器拿到就能打印；
//  2. 文档与 JSON 走**同一套授权**：拿不到变更的人，导出同样拿不到。
//     一个能被导出的未授权视图，比没有导出更糟——它会把「能看」变成
//     「能拿走」。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

func seedTicketRun(t *testing.T, store state.Store, runID string) {
	t.Helper()
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: runID, WorkflowName: "db-change.yaml", Status: "approved",
		Priority: "high", IncidentID: "prod", Creator: "alice",
		PlanHash: "v2:abc", CreatedAt: now, UpdatedAt: now,
	}))
	p := &plan.Plan{
		ID: runID,
		Batches: []plan.Batch{{Index: 0, Targets: []string{"db-1"}, Steps: []plan.PlanStep{{
			Name: "migrate", Module: "mysql", Action: "query",
			Args: map[string]any{"sql": "ALTER TABLE t ADD COLUMN c INT"},
		}}}},
		TotalTargets: 1,
		SQLReview: &plan.SQLReviewSummary{
			Verdict: "warn", WarnCount: 1,
			Findings: []plan.SQLReviewFinding{{
				Step: "migrate", Rule: "SR-05", Severity: "warn", Statement: 1,
				Message: "ADD COLUMN without a default rewrites the table",
			}},
		},
	}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.NoError(t, store.UpdateRunPlan(ctx, runID, string(raw), "v2:abc", now))
}

func TestRESTTicketMarkdownExportReturnsTheDocument(t *testing.T) {
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-1")

	resp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-1/ticket?format=markdown", "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/markdown")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	doc := string(body)
	assert.Contains(t, doc, "# 数据库变更发布单")
	assert.Contains(t, doc, "run-export-1")
	assert.Contains(t, doc, "ALTER TABLE t ADD COLUMN c INT")
	assert.Contains(t, doc, "SR-05")
	assert.Contains(t, doc, "## 6. 审计链验证")
}

func TestRESTTicketMarkdownAndJSONAgreeOnTheFacts(t *testing.T) {
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-2")

	mdResp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-2/ticket?format=markdown", "")
	defer func() { _ = mdResp.Body.Close() }()
	md, err := io.ReadAll(mdResp.Body)
	require.NoError(t, err)

	jsonResp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-2/ticket", "")
	defer func() { _ = jsonResp.Body.Close() }()
	raw, err := io.ReadAll(jsonResp.Body)
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	change, _ := payload["change"].(map[string]any)

	// The two renderings must carry the same facts: a printed sheet that
	// disagrees with the screen is worse than no printed sheet.
	assert.Contains(t, string(md), change["id"].(string))
	assert.Contains(t, string(md), change["planHash"].(string))
	assert.Contains(t, string(md), change["workflow"].(string))
}

func TestRESTTicketMarkdownWorksUnderTheAPIPrefix(t *testing.T) {
	// The console's axios client uses baseURL "/api/v1", so the documented
	// path is /api/v1/changes/<id>/ticket. The dispatch router strips the
	// prefix and hands the request to the same handler, so the export has to
	// work under the prefix too — otherwise the button in the UI would 400
	// while the CLI worked.
	gw, _, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-4")

	apiSrv := httptest.NewServer(gw.dispatch())
	t.Cleanup(apiSrv.Close)

	resp := doReq(t, http.MethodGet, apiSrv.URL+"/api/v1/changes/run-export-4/ticket?format=markdown", "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "# 数据库变更发布单")
	assert.Contains(t, string(body), "run-export-4")
}

func TestRESTTicketPrintableHTMLIsASelfContainedSheet(t *testing.T) {
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-html")

	resp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-html/ticket?format=html", "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Equal(t, "nosniff", resp.Header.Get("X-Content-Type-Options"))
	// The sheet is inert: the CSP says no external resources, and the only
	// script allowed is the static print call.
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'none'")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	doc := string(body)
	assert.Contains(t, doc, "<!doctype html>")
	assert.Contains(t, doc, "<h2>1. SQL 与审核</h2>")
	assert.Contains(t, doc, "ALTER TABLE t ADD COLUMN c INT")
	assert.Contains(t, doc, "@media print")
	assert.NotContains(t, doc, "<link")
	// Without ?print=1 the document must NOT open a dialog on load.
	assert.NotContains(t, doc, "window.print()")
}

func TestRESTTicketPrintableHTMLAutoPrintsOnRequest(t *testing.T) {
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-print")

	resp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-print/ticket?format=html&print=1", "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "window.print()",
		"the console's print button asks for the dialog to open itself")
}

func TestRESTTicketRejectsAnUnknownFormat(t *testing.T) {
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	seedTicketRun(t, store, "run-export-3")

	resp := doReq(t, http.MethodGet, srv.URL+"/changes/run-export-3/ticket?format=pdf", "")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"an unknown format must be refused, not silently treated as JSON")
}
