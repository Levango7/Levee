// opsmesh_report_test.go — the OpsMesh result-report wire: alert-driven fix
// outcomes are POSTed to /api/v1/alerts/{id}/resolution (§6.2 of the
// integration design); runs without an alert id are not reported.
package grpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/opsmesh"

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
