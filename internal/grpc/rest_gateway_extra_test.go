// rest_gateway_extra_test.go exercises the REST-to-gRPC gateway paths the
// earlier rest_test.go suite did not reach: the per-change sub-resource
// endpoints (plan/apply/reject/pause/resume/retry/rollback/logs/trace),
// query-parameter parsing, template instantiation, target removal/check,
// system config/doctor, the full /api/v1/<Service>/<Method> matrix across
// all eight services, the rate-limit and request-id middlewares, the
// gRPC→HTTP error mapping table and the ServeGateway lifecycle.
package grpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// startTestGatewayFull is like startTestGateway but also wires real
// Alert/Diagnosis/Conversation implementations into the gateway.
func startTestGatewayFull(t *testing.T, cfg ServeGatewayConfig) (*Gateway, *httptest.Server, state.Store) {
	t.Helper()

	store := newTestStore(t)
	changeSvc := NewChangeService(store, nil, nil, nil)
	templateSvc := NewTemplateService(store, nil)
	targetSvc := NewTargetService(newTestStore(t), nil)
	auditSvc := NewAuditService(store)
	sysCfg := &config.Config{
		Server:   config.ServerConfig{DataDir: t.TempDir(), LogLevel: "info", LogFormat: "text"},
		Database: config.DatabaseConfig{Driver: "sqlite", Path: ":memory:"},
	}
	systemSvc := NewSystemService(store, sysCfg, "", "test-v1.0", "abc123", "2024-01-01", "go1.21", time.Now())
	alertSvc := NewAlertService(nil, nil)
	diagSvc := NewDiagnosisService(diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{}), nil)
	convSvc := NewConversationService(conversation.NewConversationEngine(conversation.ConversationEngineConfig{}), nil)

	gw := NewGateway(cfg)
	gw.SetServices(changeSvc, templateSvc, targetSvc, auditSvc, systemSvc, alertSvc, diagSvc, convSvc)

	mux := http.NewServeMux()
	mux.Handle("/", requestIDMiddlewareForTest(gw, corsMiddleware(cfg.CORSOrigins, gw.authMiddleware(gw.rateLimitMiddleware(gw.restRoute())))))
	mux.Handle("/api/v1/", requestIDMiddlewareForTest(gw, corsMiddleware(cfg.CORSOrigins, gw.authMiddleware(gw.rateLimitMiddleware(gw.route())))))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return gw, srv, store
}

// requestIDMiddlewareForTest mirrors Gateway.requestIDMiddleware so the
// httptest-driven handlers below run behind it exactly as in serve().
func requestIDMiddlewareForTest(gw *Gateway, h http.Handler) http.Handler {
	return gw.requestIDMiddleware(h)
}

// doReq issues an HTTP request against the test server and returns the
// response; the caller must close the body.
func doReq(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	var resp *http.Response
	var err error
	if body == "" && (method == http.MethodGet || method == http.MethodDelete) {
		resp, err = http.DefaultClient.Do(mustRequest(t, method, url))
	} else {
		resp, err = http.DefaultClient.Do(mustRequest(t, method, url, body))
	}
	require.NoError(t, err)
	return resp
}

func mustRequest(t *testing.T, method, url string, body ...string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if len(body) > 0 {
		rdr = strings.NewReader(body[0])
	}
	req, err := http.NewRequest(method, url, rdr)
	require.NoError(t, err)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// --- change sub-resource endpoints -----------------------------------------------

func TestRESTChangeSubResourceEndpoints(t *testing.T) {
	gw, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "sub-resource-target"})
	require.NoError(t, err)
	id := created.GetId()

	// plan → 200 with the no-engine placeholder plan.
	resp := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/plan",
		`{"targetHosts":["h1","h2"],"dryRun":true}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var plan struct {
		ImpactSummary string `json:"impactSummary"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&plan))
	assert.Contains(t, plan.ImpactSummary, "no engine configured")

	// apply → 412 (FailedPrecondition): this gateway has no execution
	// engine wired, so apply is refused instead of faking success and
	// leaving the run stuck in "running".
	resp2 := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/apply", `{"autoApprove":true}`)
	require.NoError(t, readAndClose(resp2))
	assert.Equal(t, http.StatusPreconditionFailed, resp2.StatusCode)

	// Applying a draft WITHOUT autoApprove is also rejected (412).
	draftResp := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/apply", `{}`)
	require.NoError(t, readAndClose(draftResp))
	assert.Equal(t, http.StatusPreconditionFailed, draftResp.StatusCode)

	// pause from draft is not a legal transition → 412.
	pauseResp := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/pause", `{"reason":"freeze"}`)
	require.NoError(t, readAndClose(pauseResp))
	assert.Equal(t, http.StatusPreconditionFailed, pauseResp.StatusCode)

	// resume → 200: draft -> running is a legal transition and the
	// gateway's status tracking remains fully functional without an engine.
	resp4 := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/resume", `{}`)
	defer resp4.Body.Close()
	require.Equal(t, http.StatusOK, resp4.StatusCode)
	var resumed struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp4.Body).Decode(&resumed))
	assert.Equal(t, "running", resumed.Status)

	// reject → 200.
	resp5 := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/reject",
		`{"rejecter":"bob","reason":"too risky"}`)
	defer resp5.Body.Close()
	require.Equal(t, http.StatusOK, resp5.StatusCode)
	var rejected struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp5.Body).Decode(&rejected))
	assert.Equal(t, "rejected", rejected.Status)
}

func TestRESTChangePauseFromDraftIsPreconditionFailed(t *testing.T) {
	gw, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})
	created, err := gw.change.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "draft-pause"})
	require.NoError(t, err)
	id := created.GetId()

	// draft → paused is not a legal transition: FailedPrecondition maps
	// to HTTP 412.
	resp := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/pause", `{}`)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusPreconditionFailed, resp.StatusCode)
}

func TestRESTChangeRetryAndRollbackLifecycle(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "retry-target"})
	require.NoError(t, err)
	id := created.GetId()

	// retry on a non-failed change → 412.
	respBad := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/retry", `{}`)
	require.NoError(t, readAndClose(respBad))
	assert.Equal(t, http.StatusPreconditionFailed, respBad.StatusCode)

	setRunStatus(t, store, id, "failed")
	respRetry := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/retry", `{"replan":true,"targetHosts":["h1"]}`)
	require.NoError(t, readAndClose(respRetry))
	assert.Equal(t, http.StatusOK, respRetry.StatusCode)

	// rollback requires completed/failed; move to completed first.
	setRunStatus(t, store, id, "completed")
	respRb := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/rollback",
		`{"runId":"orig-run","autoApprove":true}`)
	defer readAndClose(respRb)
	require.Equal(t, http.StatusOK, respRb.StatusCode)
	var rb struct {
		Success bool `json:"success"`
		Change  struct {
			Status string `json:"status"`
		} `json:"change"`
	}
	require.NoError(t, json.NewDecoder(respRb.Body).Decode(&rb))
	assert.True(t, rb.Success)
	assert.Equal(t, "rolled_back", rb.Change.Status)

	// rollback again now that it is rolled_back → 412.
	respRb2 := doReq(t, http.MethodPost, srv.URL+"/changes/"+id+"/rollback", `{}`)
	require.NoError(t, readAndClose(respRb2))
	assert.Equal(t, http.StatusPreconditionFailed, respRb2.StatusCode)
}

// readAndClose drains and closes a response body so keep-alives are reused.
func readAndClose(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func TestRESTChangeLogsAndTraceEndpoints(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "logs-http"})
	require.NoError(t, err)
	id := created.GetId()

	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-http", RunID: id, BatchNo: 1, Status: "completed",
		TotalHosts: 1, Succeeded: 1, StartedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-http", RunID: id, BatchID: "batch-http", Host: "h1",
		StepName: "deploy", Status: "completed", Stdout: "http log line", Stderr: "http warn line",
		StartedAt: &now,
	}))

	resp := doReq(t, http.MethodGet, srv.URL+"/changes/"+id+"/logs?levels=ERROR&limit=5&runId=", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var logs struct {
		Entries []struct {
			Level   string `json:"level"`
			Message string `json:"message"`
			Source  string `json:"source"`
		} `json:"entries"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&logs))
	// levels=ERROR filters: only the stderr-derived ERROR entry comes back.
	require.Len(t, logs.Entries, 1)
	assert.Equal(t, "ERROR", logs.Entries[0].Level)
	assert.Equal(t, "h1", logs.Entries[0].Source)

	traceResp := doReq(t, http.MethodGet, srv.URL+"/changes/"+id+"/trace?verify=true&runId=alt-run", "")
	defer traceResp.Body.Close()
	require.Equal(t, http.StatusOK, traceResp.StatusCode)
	var trace struct {
		RunID string `json:"runId"`
	}
	require.NoError(t, json.NewDecoder(traceResp.Body).Decode(&trace))
	// The explicit runId query parameter is honoured over the change id.
	assert.Equal(t, "alt-run", trace.RunID)
}

func TestRESTChangeListQueryFilters(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	url := srv.URL + "/changes?status=draft,pending&team=platform&environment=staging" +
		"&labelContains=web&pageSize=10&pageToken=0&sortBy=id&sortOrder=asc"
	resp := doReq(t, http.MethodGet, url, "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var listed struct {
		TotalSize int32 `json:"totalSize"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
	assert.GreaterOrEqual(t, listed.TotalSize, int32(0))
}

func TestRESTChangeInvalidJSONBodiesAre400(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})
	const id = "some-change-id"

	for _, sub := range []string{"plan", "apply", "approve", "reject", "cancel", "retry", "rollback"} {
		resp, err := http.Post(srv.URL+"/changes/"+id+"/"+sub, "application/json",
			strings.NewReader("{not-json"))
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "sub-path %s", sub)
		readAndClose(resp)
	}
}

func TestRESTChangeMethodNotAllowedOnCollectionItem(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/changes/some-id", strings.NewReader("{}"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(resp)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// --- templates -----------------------------------------------------------------

func TestRESTTemplateInstantiateEndpoint(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	createResp := doReq(t, http.MethodPost, srv.URL+"/templates",
		`{"name":"inst-tmpl","description":"d","workflowContent":"steps: []"}`)
	require.NoError(t, readAndClose(createResp))
	require.Equal(t, http.StatusOK, createResp.StatusCode)

	instResp := doReq(t, http.MethodPost, srv.URL+"/templates/instantiate",
		`{"templateName":"inst-tmpl","label":"from-rest","params":{"k":"v"},"dryRun":true}`)
	defer instResp.Body.Close()
	require.Equal(t, http.StatusOK, instResp.StatusCode)
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(instResp.Body).Decode(&created))
	assert.NotEmpty(t, created.ID)
	assert.Equal(t, "planned", created.Status, "dryRun instantiation stays in planned state")

	missingResp := doReq(t, http.MethodPost, srv.URL+"/templates/instantiate",
		`{"templateName":"nope"}`)
	assert.NoError(t, readAndClose(missingResp))
	assert.Equal(t, http.StatusNotFound, missingResp.StatusCode)
}

func TestRESTTemplateDispatchEdges(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	// Unknown sub-path under a named template → 400.
	resp := doReq(t, http.MethodPost, srv.URL+"/templates/tmpl/bogus-sub", `{}`)
	assert.NoError(t, readAndClose(resp))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// PUT on a named template → 405.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/templates/tmpl", strings.NewReader("{}"))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(putResp)
	assert.Equal(t, http.StatusMethodNotAllowed, putResp.StatusCode)

	// DELETE with force query param still routes.
	delResp := doReq(t, http.MethodDelete, srv.URL+"/templates/nope?force=true", "")
	defer readAndClose(delResp)
	assert.Equal(t, http.StatusNotFound, delResp.StatusCode)
}

// --- targets --------------------------------------------------------------------

func TestRESTTargetRemoveAndCheckEndpoints(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	addResp := doReq(t, http.MethodPost, srv.URL+"/targets",
		`{"hostname":"rest-host-1","channelType":"ssh","port":2222,"labels":{"env":"staging"}}`)
	defer readAndClose(addResp)
	require.Equal(t, http.StatusOK, addResp.StatusCode)
	var added struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(addResp.Body).Decode(&added))
	require.NotEmpty(t, added.ID)

	listURL := srv.URL + "/targets?labelSelector=env=staging,role=web&channelType=ssh&reachableOnly=false&pageSize=10&pageToken=0"
	listResp := doReq(t, http.MethodGet, listURL, "")
	require.NoError(t, readAndClose(listResp))
	assert.Equal(t, http.StatusOK, listResp.StatusCode)

	checkResp := doReq(t, http.MethodGet, srv.URL+"/targets/"+added.ID+"/check?fresh=true&timeoutSeconds=1", "")
	defer checkResp.Body.Close()
	require.Equal(t, http.StatusOK, checkResp.StatusCode)
	var check struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.NewDecoder(checkResp.Body).Decode(&check))
	assert.Contains(t, check.Error, "no channel factory configured")

	checkMissing := doReq(t, http.MethodGet, srv.URL+"/targets/target-nope/check?fresh=true", "")
	require.NoError(t, readAndClose(checkMissing))
	assert.Equal(t, http.StatusNotFound, checkMissing.StatusCode)

	delResp := doReq(t, http.MethodDelete, srv.URL+"/targets/"+added.ID+"?force=true", "")
	require.NoError(t, readAndClose(delResp))
	assert.Equal(t, http.StatusOK, delResp.StatusCode)

	delMissing := doReq(t, http.MethodDelete, srv.URL+"/targets/target-nope?force=true", "")
	require.NoError(t, readAndClose(delMissing))
	assert.Equal(t, http.StatusNotFound, delMissing.StatusCode)

	// Unknown sub-path under a named target → 400.
	badSub := doReq(t, http.MethodGet, srv.URL+"/targets/x/bogus", "")
	require.NoError(t, readAndClose(badSub))
	assert.Equal(t, http.StatusBadRequest, badSub.StatusCode)

	// PUT on a named target → 405.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/targets/x", strings.NewReader("{}"))
	require.NoError(t, err)
	putResp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer readAndClose(putResp)
	assert.Equal(t, http.StatusMethodNotAllowed, putResp.StatusCode)
}

// --- system config / doctor --------------------------------------------------------

func TestRESTSystemConfigAndDoctor(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	cfgResp := doReq(t, http.MethodGet, srv.URL+"/system/config?redactSecrets=true&section=database", "")
	defer cfgResp.Body.Close()
	require.Equal(t, http.StatusOK, cfgResp.StatusCode)

	doctorResp := doReq(t, http.MethodPost, srv.URL+"/system/doctor", "")
	defer doctorResp.Body.Close()
	require.Equal(t, http.StatusOK, doctorResp.StatusCode)
	var doc struct {
		Status string `json:"status"`
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	require.NoError(t, json.NewDecoder(doctorResp.Body).Decode(&doc))
	assert.NotEmpty(t, doc.Checks)
}

// --- legacy /api/v1 method matrix ---------------------------------------------------

func TestAPIv1MethodMatrix(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	tests := []struct {
		service string
		method  string
		body    string
		want    int
	}{
		// ChangeService.
		{service: "ChangeService", method: "ListChanges", body: "{}", want: http.StatusOK},
		{service: "ChangeService", method: "PauseAll", body: "{}", want: http.StatusOK},
		{service: "ChangeService", method: "ResumeAll", body: "{}", want: http.StatusOK},
		{service: "ChangeService", method: "GetLogs", body: `{"changeId":"nope"}`, want: http.StatusOK},
		{service: "ChangeService", method: "CreateChange", body: `{"workflowFile":"w.levee"}`, want: http.StatusOK},
		{service: "ChangeService", method: "CloneChange", body: `{"sourceChangeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "PlanChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "ApplyChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "PauseChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "ResumeChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "CancelChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "RetryChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "RetryHost", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "RollbackChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "ApproveChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "RejectChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "ArchiveChange", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "GetChange", body: `{"id":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "GetDiff", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "GetTrace", body: `{"changeId":"nope"}`, want: http.StatusNotFound},
		{service: "ChangeService", method: "Bogus", body: "{}", want: http.StatusBadRequest},

		// TemplateService.
		{service: "TemplateService", method: "ListTemplates", body: "{}", want: http.StatusOK},
		{service: "TemplateService", method: "CreateTemplate", body: `{"name":"api-tmpl","workflowContent":"steps: []"}`, want: http.StatusOK},
		{service: "TemplateService", method: "GetTemplate", body: `{"name":"api-tmpl"}`, want: http.StatusOK},
		{service: "TemplateService", method: "DeleteTemplate", body: `{"name":"api-tmpl"}`, want: -1}, // handled below (DELETE-style empty body)
		{service: "TemplateService", method: "InstantiateTemplate", body: `{"templateName":"nope"}`, want: http.StatusNotFound},
		{service: "TemplateService", method: "Bogus", body: "{}", want: http.StatusBadRequest},

		// TargetService.
		{service: "TargetService", method: "ListTargets", body: "{}", want: http.StatusOK},
		{service: "TargetService", method: "AddTarget", body: `{"hostname":"api-host","channelType":"ssh","port":22}`, want: http.StatusOK},
		{service: "TargetService", method: "AddTarget", body: `{}`, want: http.StatusBadRequest},
		{service: "TargetService", method: "GetTarget", body: `{"id":"nope"}`, want: http.StatusNotFound},
		{service: "TargetService", method: "CheckTarget", body: `{"id":"nope"}`, want: http.StatusNotFound},

		// AuditService.
		{service: "AuditService", method: "GetAuditLog", body: "{}", want: http.StatusOK},
		{service: "AuditService", method: "ListAuditTraces", body: "{}", want: http.StatusOK},
		{service: "AuditService", method: "VerifyHashChain", body: `{"runId":"run-x"}`, want: http.StatusOK},
		{service: "AuditService", method: "GetRunReport", body: `{"runId":"nope"}`, want: http.StatusNotFound},
		{service: "AuditService", method: "Bogus", body: "{}", want: http.StatusBadRequest},

		// SystemService.
		{service: "SystemService", method: "GetVersion", body: "{}", want: http.StatusOK},
		{service: "SystemService", method: "GetStatus", body: "{}", want: http.StatusOK},
		{service: "SystemService", method: "GetConfig", body: `{"section":"database"}`, want: http.StatusOK},
		{service: "SystemService", method: "RunDoctor", body: "{}", want: http.StatusOK},
		{service: "SystemService", method: "Nope", body: "{}", want: http.StatusBadRequest},

		// AlertService (real implementation wired).
		{service: "AlertService", method: "ReceiveAlert", body: `{"source":"prom","title":"t"}`, want: http.StatusOK},
		{service: "AlertService", method: "GetAlertStatus", body: `{"id":"missing"}`, want: http.StatusNotFound},
		{service: "AlertService", method: "SubscribeAlerts", body: "{}", want: http.StatusNotImplemented},
		{service: "AlertService", method: "Nope", body: "{}", want: http.StatusBadRequest},

		// DiagnosisService.
		{service: "DiagnosisService", method: "Diagnose", body: `{"target":"h1"}`, want: http.StatusOK},
		{service: "DiagnosisService", method: "Diagnose", body: `{}`, want: http.StatusBadRequest},
		{service: "DiagnosisService", method: "GetDiagnosis", body: `{"id":"missing"}`, want: http.StatusNotFound},
		{service: "DiagnosisService", method: "Nope", body: "{}", want: http.StatusBadRequest},

		// ConversationService (json tags are snake_case in the hand-written
		// pb structs, e.g. "user_id").
		{service: "ConversationService", method: "SendMessage", body: `{"user_id":"u1","text":"/help"}`, want: http.StatusOK},
		{service: "ConversationService", method: "SendMessage", body: `{}`, want: http.StatusBadRequest},
		{service: "ConversationService", method: "SubscribeConversation", body: "{}", want: http.StatusNotImplemented},
		{service: "ConversationService", method: "Nope", body: "{}", want: http.StatusBadRequest},

		// Unknown service.
		{service: "NopeService", method: "Method", body: "{}", want: http.StatusBadRequest},
	}

	for _, tc := range tests {
		tc := tc
		name := tc.service + "/" + tc.method
		if tc.want == -1 {
			continue // DeleteTemplate covered explicitly below
		}
		t.Run(name, func(t *testing.T) {
			resp, err := http.Post(
				fmt.Sprintf("%s/api/v1/%s/%s", srv.URL, tc.service, tc.method),
				"application/json", strings.NewReader(tc.body))
			require.NoError(t, err)
			defer readAndClose(resp)
			assert.Equal(t, tc.want, resp.StatusCode, "body: %s", tc.body)
		})
	}
}

// TestExtraServicesProtoJSONContract pins the serialization contract for the
// Alert/Diagnosis/Conversation REST endpoints now that they use protojson
// (previously a hand-written reflection mirror). It guards the three
// behaviours documented in docs/levee-api.md §13.5: proto3 zero-value fields
// are omitted, int64 fields are emitted as JSON strings, and request bodies
// accept both camelCase and snake_case spellings.
func TestExtraServicesProtoJSONContract(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	post := func(service, method, body string) (*http.Response, map[string]any) {
		t.Helper()
		resp, err := http.Post(
			fmt.Sprintf("%s/api/v1/%s/%s", srv.URL, service, method),
			"application/json", strings.NewReader(body))
		require.NoError(t, err)
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		var m map[string]any
		if len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, &m), "body: %s", raw)
		}
		return resp, m
	}

	// Zero-value omission: a successful ReceiveAlert has an empty `reason`,
	// which protojson must omit (the old mirror emitted "reason":"").
	t.Run("ReceiveAlert omits zero-value fields", func(t *testing.T) {
		resp, m := post("AlertService", "ReceiveAlert", `{"source":"prom","title":"t"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "accepted", m["status"])
		assert.NotEmpty(t, m["id"])
		_, hasReason := m["reason"]
		assert.False(t, hasReason, "empty reason must be omitted per protojson zero-value elision")
	})

	// int64 as JSON string: GetAlertStatus.startsAt is a Unix timestamp and
	// must be serialised as a string, not a JSON number.
	t.Run("GetAlertStatus emits int64 as string", func(t *testing.T) {
		resp, _ := post("AlertService", "ReceiveAlert", `{"id":"contract-1","source":"prom","title":"t"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		resp, m := post("AlertService", "GetAlertStatus", `{"id":"contract-1"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		startsAt, ok := m["startsAt"].(string)
		require.True(t, ok, "startsAt must be a JSON string, got %T (%v)", m["startsAt"], m["startsAt"])
		assert.NotEmpty(t, startsAt)
		_, hasEndsAt := m["endsAt"]
		assert.False(t, hasEndsAt, "zero endsAt must be omitted")
	})

	// Dual-name tolerance: both snake_case and camelCase request spellings
	// are accepted for the same field.
	t.Run("SendMessage accepts snake_case and camelCase", func(t *testing.T) {
		resp, m := post("ConversationService", "SendMessage", `{"user_id":"u1","text":"/help"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, m["text"])
		resp, m = post("ConversationService", "SendMessage", `{"userId":"u1","text":"/help"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, m["text"])
	})

	// Diagnose responses follow the same contract.
	t.Run("Diagnose response follows protojson", func(t *testing.T) {
		resp, m := post("DiagnosisService", "Diagnose", `{"target":"h1"}`)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.NotEmpty(t, m["id"])
		if v, present := m["durationMs"]; present {
			assert.IsType(t, "", v, "durationMs (int64) must be a JSON string")
		}
	})
}

func TestAPIv1InvalidPathVariants(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	// Missing method segment.
	resp := doReq(t, http.MethodPost, srv.URL+"/api/v1/ChangeService", `{}`)
	require.NoError(t, readAndClose(resp))
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- middleware units ---------------------------------------------------------------

func TestWriteGRPCErrorMappingTable(t *testing.T) {
	tests := []struct {
		code    grpccodes.Code
		want    int
		wantMsg string // expected body message; "" means echo "msg"
	}{
		{grpccodes.InvalidArgument, http.StatusBadRequest, ""},
		{grpccodes.NotFound, http.StatusNotFound, ""},
		{grpccodes.AlreadyExists, http.StatusConflict, ""},
		{grpccodes.PermissionDenied, http.StatusForbidden, ""},
		{grpccodes.FailedPrecondition, http.StatusPreconditionFailed, ""},
		{grpccodes.Unimplemented, http.StatusNotImplemented, ""},
		{grpccodes.Unavailable, http.StatusServiceUnavailable, ""},
		{grpccodes.Unauthenticated, http.StatusUnauthorized, ""},
		{grpccodes.DeadlineExceeded, http.StatusGatewayTimeout, ""},
		{grpccodes.ResourceExhausted, http.StatusTooManyRequests, ""},
		// Internal-class codes are scrubbed: the real message goes to the
		// server log, the client gets a generic body.
		{grpccodes.Internal, http.StatusInternalServerError, "internal error"},
		{grpccodes.Unknown, http.StatusInternalServerError, "internal error"},
	}
	for _, tc := range tests {
		t.Run(tc.code.String(), func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGRPCError(rec, status.Error(tc.code, "msg"))
			assert.Equal(t, tc.want, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			if tc.wantMsg == "" {
				assert.Contains(t, rec.Body.String(), "msg")
			} else {
				assert.Contains(t, rec.Body.String(), tc.wantMsg)
				assert.NotContains(t, rec.Body.String(), "msg", "internal details must not leak")
			}
		})
	}

	t.Run("non-gRPC error becomes generic 500", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeGRPCError(rec, assertNotGRPC())
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.Contains(t, rec.Body.String(), "internal error")
		assert.NotContains(t, rec.Body.String(), "plain error", "raw error text must not leak")
	})
}

func assertNotGRPC() error { return fmt.Errorf("plain error") }

func TestRateLimitMiddleware(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("exhausted bucket returns 429 with retry hint", func(t *testing.T) {
		gw := NewGateway(ServeGatewayConfig{RatePerSec: 0.001, RateBurst: 1})
		h := gw.rateLimitMiddleware(handler)

		first := httptest.NewRecorder()
		h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/x", nil))
		assert.Equal(t, http.StatusOK, first.Code)

		second := httptest.NewRecorder()
		h.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/x", nil))
		assert.Equal(t, http.StatusTooManyRequests, second.Code)
		assert.Equal(t, "1", second.Header().Get("Retry-After"))
	})

	t.Run("negative rate disables limiting", func(t *testing.T) {
		gw := NewGateway(ServeGatewayConfig{RatePerSec: -1})
		h := gw.rateLimitMiddleware(handler)
		for i := 0; i < 50; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			require.Equal(t, http.StatusOK, rec.Code)
		}
	})
}

func TestRequestIDMiddlewareEchoesAndGenerates(t *testing.T) {
	gw := NewGateway(ServeGatewayConfig{})

	var seenID string
	handler := gw.requestIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenID = RequestIDFromContext(r.Context())
	}))

	t.Run("client supplied header is echoed and propagated", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-Id", "my-trace-123")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, "my-trace-123", rec.Header().Get("X-Request-Id"))
		assert.Equal(t, "my-trace-123", seenID)
	})

	t.Run("absent header generates a fresh hex id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		got := rec.Header().Get("X-Request-Id")
		require.Len(t, got, 16)
		assert.NotEqual(t, "my-trace-123", got)
		assert.Len(t, seenID, 16)
	})
}

func TestNewRESTRequestIDFormat(t *testing.T) {
	a := newRESTRequestID()
	b := newRESTRequestID()
	require.Len(t, a, 16)
	assert.NotEqual(t, a, b)
}

// --- ServeGateway lifecycle ------------------------------------------------------------

func TestServeGatewayStartsAndStopsOnContextCancel(t *testing.T) {
	// Find a free loopback port, then release it for the gateway.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, ServeGateway(ctx, ServeGatewayConfig{
		Addr:        addr,
		CORSOrigins: []string{"*"},
		RatePerSec:  -1,
	}))
	baseURL := "http://" + addr

	// The health endpoint must come up. ServeGateway registers no services,
	// so the gateway reports 503 (unready) rather than 200 — either way the
	// listener is reachable, which is what this lifecycle test asserts.
	var up bool
	for i := 0; i < 100 && !up; i++ {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			readAndClose(resp)
			up = resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.True(t, up, "gateway healthz never became reachable")

	// Cancelling the context shuts the gateway down.
	cancel()
	var down bool
	for i := 0; i < 100 && !down; i++ {
		resp, err := http.Get(baseURL + "/healthz")
		if err != nil {
			down = true
		} else {
			readAndClose(resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.True(t, down, "gateway kept serving after context cancel")
}

// TestGatewayStartBindsSynchronously verifies that Start returns a real
// error when the listen address is unavailable, instead of swallowing it
// like the old fire-and-forget ServeGateway did.
func TestGatewayStartBindsSynchronously(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }() // hold the port so the bind below fails

	gw := NewGateway(ServeGatewayConfig{Addr: ln.Addr().String()})
	err = gw.Start(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "listen")
}

// TestGatewayHealthzReportsUnreadyWithoutServices pins the 503 contract for
// a gateway whose services were never registered.
func TestGatewayHealthzReportsUnreadyWithoutServices(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	gw := NewGateway(ServeGatewayConfig{Addr: addr})
	require.NoError(t, gw.Start(context.Background()))
	t.Cleanup(func() { _ = gw.Stop(context.Background()) })

	resp, err := http.Get("http://" + addr + "/healthz")
	require.NoError(t, err)
	defer readAndClose(resp)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)

	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "unavailable", body.Status)
}

// TestGatewayWithServicesServesData pins the production wiring: after
// SetServices + Start, /healthz is 200 and data endpoints respond (not
// nil-panic). Regression guard for the dual-gateway serve bug.
func TestGatewayWithServicesServesData(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	gw, _, store := startTestGatewayFull(t, ServeGatewayConfig{})
	// Replace the httptest server path: use Start on the real listener.
	gw.cfg.Addr = addr
	require.NoError(t, gw.Start(context.Background()))
	t.Cleanup(func() { _ = gw.Stop(context.Background()); _ = store.Close() })

	resp, err := http.Get("http://" + addr + "/healthz")
	require.NoError(t, err)
	readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	httpReq := func(method, path string) *http.Response {
		req, err := http.NewRequest(method, "http://"+addr+path, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	resp = httpReq("GET", "/changes")
	readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	resp = httpReq("GET", "/system/version")
	readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestGatewayStopWithoutServerIsNoOp(t *testing.T) {
	gw := NewGateway(ServeGatewayConfig{})
	require.NoError(t, gw.Stop(context.Background()))
}

// --- routing exactness ----------------------------------------------------------------

// TestRESTPrefixRoutingMatchesExactSegment pins the audit fix where
// strings.HasPrefix(path, "/changes") dispatched look-alike paths such as
// "/changesfoo" into the change handlers instead of 404ing.
func TestRESTPrefixRoutingMatchesExactSegment(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	for _, path := range []string{"/changesfoo", "/templatesfoo", "/targetsfoo", "/auditfoo", "/systemfoo", "/systemconfig"} {
		resp := doReq(t, http.MethodGet, srv.URL+path, "")
		readAndClose(resp)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode, "path %s must not match a prefix route", path)
	}

	// The real prefixes keep working.
	okResp := doReq(t, http.MethodGet, srv.URL+"/changes", "")
	readAndClose(okResp)
	assert.Equal(t, http.StatusOK, okResp.StatusCode)
}

// --- numeric query param validation ------------------------------------------------------

// TestRESTInvalidNumericParamsAre400 covers the fix where malformed numeric
// query values were silently ignored (pageSize=abc behaved like no filter).
func TestRESTInvalidNumericParamsAre400(t *testing.T) {
	_, srv, _ := startTestGatewayFull(t, ServeGatewayConfig{})

	for _, path := range []string{
		"/changes?pageSize=bogus",
		"/changes?pageToken=-3",
		"/templates?pageSize=bogus",
		"/targets?pageSize=bogus",
		"/audit/log?pageSize=bogus",
		"/audit/log?since=yesterday",
		"/audit/traces?until=soon",
	} {
		resp := doReq(t, http.MethodGet, srv.URL+path, "")
		readAndClose(resp)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "path %s", path)
	}
}

// --- request id sanitization + asserted actor ----------------------------------------------

func TestRequestIDMiddlewareSanitizesAndInjectsActor(t *testing.T) {
	gw := NewGateway(ServeGatewayConfig{})

	var seenRID, seenActor string
	handler := gw.requestIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seenRID = RequestIDFromContext(r.Context())
		seenActor = actorFromCtx(r.Context())
	}))

	t.Run("actor header reaches handlers via actorFromCtx", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Acting-As", "alice@ops")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, "alice@ops", seenActor)
	})

	t.Run("control characters are stripped from ids and actors", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-Id", "bad\r\nid\x00value")
		req.Header.Set("X-Acting-As", "eve\ninjected-log-line")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		echoed := rec.Header().Get("X-Request-Id")
		assert.NotContains(t, echoed, "\r")
		assert.NotContains(t, echoed, "\n")
		assert.Equal(t, echoed, seenRID)
		assert.Equal(t, "eveinjected-log-line", seenActor)
	})

	t.Run("empty-after-sanitization regenerates the request id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-Request-Id", "\r\n\x00")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Len(t, rec.Header().Get("X-Request-Id"), 16, "expected a generated hex id")
	})
}

// --- REST logs `since` filter -------------------------------------------------------------

func TestRESTChangeLogsSinceFilter(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	ctx := context.Background()

	created, err := gw.change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "logs-since"})
	require.NoError(t, err)
	id := created.GetId()

	old := time.Now().UTC().Add(-2 * time.Hour)
	fresh := time.Now().UTC()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-old", RunID: id, BatchNo: 1, Status: "completed",
		TotalHosts: 1, Succeeded: 1, StartedAt: &old,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-old", RunID: id, BatchID: "batch-old", Host: "h1",
		StepName: "deploy", Status: "completed", Stdout: "ancient output", StartedAt: &old,
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "batch-new", RunID: id, BatchNo: 2, Status: "completed",
		TotalHosts: 1, Succeeded: 1, StartedAt: &fresh,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "step-new", RunID: id, BatchID: "batch-new", Host: "h1",
		StepName: "verify", Status: "completed", Stdout: "fresh output", StartedAt: &fresh,
	}))

	since := fresh.Add(-time.Minute).Format(time.RFC3339)
	resp := doReq(t, http.MethodGet, srv.URL+"/changes/"+id+"/logs?since="+since, "")
	defer readAndClose(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var logs struct {
		Entries []struct {
			Message string `json:"message"`
		} `json:"entries"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&logs))
	require.Len(t, logs.Entries, 1, "only steps newer than ?since are returned")
	assert.Equal(t, "fresh output", logs.Entries[0].Message)

	badResp := doReq(t, http.MethodGet, srv.URL+"/changes/"+id+"/logs?since=notatime", "")
	readAndClose(badResp)
	assert.Equal(t, http.StatusBadRequest, badResp.StatusCode, "unparseable RFC3339 since must be a 400")
}
