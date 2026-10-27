// opsmesh_report_test.go — the OpsMesh result-report wire: alert-driven fix
// outcomes are POSTed to /api/v1/alerts/{id}/resolution (§6.2 of the
// integration design); runs without an alert id are not reported.
package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/opsmesh"
	"github.com/nexus/levee/internal/runstatus"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyChange_ReportsOpsMeshResultForAlertDrivenRun(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := &racingEngine{runID: "exec-om", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	svc.SetOpsMeshReporter(opsmesh.NewOpsMeshClient(opsmesh.OpsMeshClientConfig{
		BaseURL: srv.URL, APIKey: "test-key",
	}))

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "om-report"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = "restart-nginx"
	run.Params = `{"alert_id":"alert-77","target":"web-1"}`
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.NoError(t, err)

	assert.Equal(t, "/api/v1/alerts/alert-77/resolution", gotPath,
		"the report must hit the resolution endpoint of the alert")
	assert.Equal(t, "Bearer test-key", gotAuth)
	require.NotNil(t, gotBody, "a report body must have been sent")
	assert.Equal(t, "alert-77", gotBody["alert_id"])
	assert.Equal(t, true, gotBody["success"])
	assert.Equal(t, created.GetId(), gotBody["workflow_id"])
}

func TestApplyChange_SkipsOpsMeshReportWithoutAlertID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	engine := &racingEngine{runID: "exec-om2", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	svc.SetOpsMeshReporter(opsmesh.NewOpsMeshClient(opsmesh.OpsMeshClientConfig{
		BaseURL: srv.URL, APIKey: "k",
	}))

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "om-skip"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	// No alert_id in params: a manual change has no platform alert to resolve.
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = "restart-nginx"
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.NoError(t, err)
	assert.False(t, called, "a run without an alert id must not be reported to the platform")
}

func TestApplyChange_OpsMeshReportFailureDoesNotFailTheRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	engine := &racingEngine{runID: "exec-om3", runSuccess: true}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	svc.SetOpsMeshReporter(opsmesh.NewOpsMeshClient(opsmesh.OpsMeshClientConfig{
		BaseURL: srv.URL, APIKey: "k",
	}))

	ctx := context.Background()
	created, err := svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "om-fail"})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = "restart-nginx"
	run.Params = `{"alert_id":"alert-88"}`
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: created.GetId(), AutoApprove: true})
	require.NoError(t, err, "a failing platform report must not fail the settled run")
}

// TestReportOpsMeshResult_Resolution404IsOneInfoNotAWarn pins the platform-gap
// branch on the caller side. The OpsMesh platform implements
// /alerts/{id}/ack and /silence but not /resolution, so the report answers 404
// for every alert-driven run. Before this change the caller logged a WARN
// ("report result failed") on every such run, which reads as if each run's
// feedback had broken. The fix classifies the 404 (opsmesh.ErrResolutionUnsupported)
// and reports it once at INFO — the log assertions here are the load-bearing
// part, because the run outcome is unchanged either way.
func TestReportOpsMeshResult_Resolution404IsOneInfoNotAWarn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	engine := &racingEngine{runID: "exec-om4", runSuccess: true}
	svc, _ := newTestChangeServiceWithEngine(t, engine.adapter())
	svc.SetOpsMeshReporter(opsmesh.NewOpsMeshClient(opsmesh.OpsMeshClientConfig{
		BaseURL: srv.URL, APIKey: "k",
	}))

	var buf bytes.Buffer
	log.InitLoggerWithWriter("info", "text", &buf)
	t.Cleanup(func() { log.InitLoggerWithWriter("info", "text", os.Stderr) })

	run := &state.Run{
		ID:           "run-404",
		WorkflowName: "restart-nginx",
		Params:       `{"alert_id":"alert-99"}`,
		Status:       runstatus.StatusCompleted,
	}
	// Two settled alert-driven runs: the gap is a platform fact, not a per-run
	// event, so it must be announced once and not on the second run.
	svc.reportOpsMeshResult(context.Background(), run, runstatus.StatusCompleted)
	svc.reportOpsMeshResult(context.Background(), run, runstatus.StatusCompleted)

	out := buf.String()
	assert.Contains(t, out, "no alert-resolution endpoint",
		"the platform gap must be stated, not hidden")
	assert.NotContains(t, out, "report result failed",
		"a known 404 must not be logged as a generic report failure")
	assert.Equal(t, 1, strings.Count(out, "no alert-resolution endpoint"),
		"the platform-gap notice must be emitted once per process, not per run")
}
