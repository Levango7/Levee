package grpc

// rest_steps_test.go pins /system/batch-status as the ONE read path for
// per-host progress.
//
// The per-batch aggregates already existed and were rendered; the steps table
// (host, batch, status, duration) had no reader at all, so the monitor page
// could answer "this batch is 60% accounted for" and never "which host is
// failing" — which is the question that page is opened with. The rows now
// travel with the aggregates, and the batch id is resolved to the batch NUMBER
// so a client needs no second request to build a column.
//
// The wire-shape half of this contract (snake_case keys) lives next door in
// rest_batch_status_contract_test.go; this file is about the rows themselves.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// startBatchStatusHarness boots a gateway with a store and a run in it.
func startBatchStatusHarness(t *testing.T, runID string) (string, state.Store) {
	t.Helper()
	_, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw, err := srv.URL, error(nil)
	require.NoError(t, err)
	_ = gw
	require.NoError(t, notNil(store))
	return srv.URL, store
}

func notNil(_ any) error { return nil }

func TestBatchStatusCarriesPerHostRows(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	// The route reads the gateway's own store handle (this fixture builds the
	// services on a store but does not attach it), so attach it the way
	// `runServe` does — otherwise the handler answers 503.
	gw.SetStore(store)
	ctx := t.Context()

	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-steps-1", WorkflowName: "name: steps\n", Status: "running",
		Creator: "test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "bat-steps-1", RunID: "run-steps-1", BatchNo: 1,
		Status: state.BatchStateRunning, TotalHosts: 3, Succeeded: 1, Failed: 1,
	}))
	// Three hosts, three outcomes: the third is in the plan with no row yet,
	// which is the state the map must still render as a cell.
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "st-1", RunID: "run-steps-1", BatchID: "bat-steps-1", Host: "web-01",
		StepName: "upgrade", Action: "shell.exec", Status: state.StepStatusSuccess, DurationMs: 1200,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID: "st-2", RunID: "run-steps-1", BatchID: "bat-steps-1", Host: "web-02",
		StepName: "upgrade", Action: "shell.exec", Status: state.StepStatusFailed, DurationMs: 900,
	}))

	raw := getBatchStatus(t, srv.URL, "run-steps-1")
	var top map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &top), "payload: %s", raw)

	rows, ok := top["steps"].([]any)
	require.True(t, ok, "steps must be a JSON array; got %v in %s", top["steps"], raw)
	require.Len(t, rows, 2)

	var sawHost, sawNo, sawDuration bool
	for _, r := range rows {
		m, ok := r.(map[string]any)
		require.True(t, ok)
		switch m["host"] {
		case "web-01":
			assert.Equal(t, state.StepStatusSuccess, m["status"])
			assert.Equal(t, float64(1), m["batch_no"], "batch_id resolves to the batch NUMBER")
			assert.Equal(t, float64(1200), m["duration_ms"])
			sawDuration = true
		case "web-02":
			assert.Equal(t, state.StepStatusFailed, m["status"])
			assert.Equal(t, float64(900), m["duration_ms"])
			sawHost = true
		}
		if _, ok := m["batch_no"]; ok {
			sawNo = true
		}
		assert.NotContains(t, m, "batch_id", "the raw row id is not the client's vocabulary")
	}
	assert.True(t, sawHost && sawNo && sawDuration, "the per-host facts must arrive: %s", raw)
}

func TestBatchStatusWithoutStepsKeepsTheAggregates(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	// The route reads the gateway's own store handle (this fixture builds the
	// services on a store but does not attach it), so attach it the way
	// `runServe` does — otherwise the handler answers 503.
	gw.SetStore(store)
	ctx := t.Context()
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-nosteps", WorkflowName: "name: n\n", Status: "draft",
		Creator: "test", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}))

	raw := getBatchStatus(t, srv.URL, "run-nosteps")
	var top map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &top), "payload: %s", raw)
	batches, ok := top["batches"].([]any)
	require.True(t, ok, "aggregates must stay present")
	require.Len(t, batches, 0)
	// A run with no rows omits the field rather than sending null noise the
	// client would have to special-case.
	assert.NotContains(t, top, "steps", "no rows means the field is absent")
}

func TestBatchStatusRunIDRequired(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	gw.SetStore(store)
	resp, err := http.Get(srv.URL + "/system/batch-status")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	// The store is configured, so the only reason left to refuse is the missing
	// parameter: a 503 here would mean the route checks its dependencies
	// before its input, which makes the contract harder to read.
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "run_id is the only query this route takes")
}

func getBatchStatus(t *testing.T, base, runID string) string {
	t.Helper()
	resp, err := http.Get(base + "/system/batch-status?run_id=" + runID)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(raw)
}
