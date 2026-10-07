package grpc

// rest_batch_status_contract_test.go pins the WIRE shape of
// GET /api/v1/system/batch-status.
//
// Why this test exists at all: the route returned `state.BatchSummary` straight
// out of `writeJSON`, and with no json tags that means Go's PascalCase field
// names — while both consumers (BatchSummaryDTO in web/src/api, read by
// ClusterView and MonitorView) spell the keys snake_case. Nothing raised an
// error: the panels simply never showed data. The Go-side seam test
// (internal/wiring/batch_summary_seam_test.go) proves the numbers are computed
// correctly; this one proves they arrive under the keys the browser reads.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestRESTBatchStatus_WireKeysAreSnakeCase(t *testing.T) {
	gw, srv, store := startTestGatewayFull(t, ServeGatewayConfig{})
	// The route reads the gateway's own store handle (this fixture builds the
	// services on a store but does not attach it), so attach it the way
	// `runServe` does — otherwise the handler answers 503 "no store configured".
	gw.SetStore(store)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-wire-1", WorkflowName: "name: wire\n", Status: "completed",
		Creator: "test", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "bat-wire-1", RunID: "run-wire-1", BatchNo: 1,
		Status: state.BatchStateCompleted, TotalHosts: 2, Succeeded: 2,
	}))

	resp := doReq(t, http.MethodGet, srv.URL+"/system/batch-status?run_id=run-wire-1", "")
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var top map[string]any
	require.NoError(t, json.Unmarshal(raw, &top), "payload: %s", raw)

	// The keys the frontend reads. PascalCase here is the regression this pins.
	for _, k := range []string{"batches", "current_batch_no", "total_batches", "done_batches"} {
		assert.Contains(t, top, k, "response must carry %q; wire was: %s", k, raw)
	}
	for _, gone := range []string{"Batches", "CurrentBatchNo", "TotalBatches", "DoneBatches"} {
		assert.NotContains(t, top, gone, "PascalCase key %q leaked back into the payload", gone)
	}

	// And inside the array, per field the monitor page renders.
	rows, ok := top["batches"].([]any)
	require.True(t, ok, "batches must be a JSON array; got %T in %s", top["batches"], raw)
	require.Len(t, rows, 1)
	b0, ok := rows[0].(map[string]any)
	require.True(t, ok)
	for _, k := range []string{"batch_no", "status", "total_hosts", "succeeded", "failed"} {
		assert.Contains(t, b0, k, "batch row must carry %q", k)
	}
	assert.Equal(t, "completed", b0["status"])

	// The value the whole chain exists to produce: a completed batch counted done.
	assert.Equal(t, float64(1), top["done_batches"], "DoneBatches must be visible to the browser under this key")
}
