package topology

// skywalking_test.go exercises the SkyWalkingCollector against an httptest
// mock server. It covers the happy path (topology projected from a GraphQL
// response), the error path (HTTP 500), the empty-response path and the
// collector Name accessor.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSkyWalkingTestServer returns an httptest.Server that always responds
// with the supplied status code and body bytes. Callers own the returned
// server and must close it.
func newSkyWalkingTestServer(status int, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
}

// TestSkyWalkingCollector_Name verifies the stable collector identifier.
func TestSkyWalkingCollector_Name(t *testing.T) {
	c := NewSkyWalkingCollector("http://example/graphql", nil)
	assert.Equal(t, "skywalking", c.Name())
}

// TestSkyWalkingCollector_Collect_Success posts a representative GraphQL
// response and asserts that the collector projects it into the unified
// Topology model, including the microsecond→millisecond latency conversion.
func TestSkyWalkingCollector_Collect_Success(t *testing.T) {
	resp := skywalkingGraphQLResponse{
		Data: skywalkingTopology{
			Nodes: []skywalkingNode{
				{ID: "1", Name: "order-svc", Type: "HTTP", Endpoint: "order:8080", Metadata: map[string]string{"lang": "go"}},
				{ID: "2", Name: "pay-svc", Type: "RPC", Endpoint: "pay:9090"},
			},
			Calls: []skywalkingCall{
				{Source: "1", Target: "2", CallCount: 100, ErrorCount: 4, AvgLatency: 12500}, // 12.5 ms
			},
		},
	}
	body, err := json.Marshal(resp)
	require.NoError(t, err)

	srv := newSkyWalkingTestServer(http.StatusOK, body)
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	tr := TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.NoError(t, err)
	require.NotNil(t, topo)

	require.Len(t, topo.Nodes, 2)
	assert.Equal(t, "order-svc", topo.Nodes[0].Name)
	assert.Equal(t, "go", topo.Nodes[0].Metadata["lang"])
	assert.Equal(t, "pay-svc", topo.Nodes[1].Name)

	require.Len(t, topo.Edges, 1)
	edge := topo.Edges[0]
	assert.Equal(t, "1", edge.Source)
	assert.Equal(t, "2", edge.Target)
	assert.Equal(t, int64(100), edge.Metric.CallCount)
	assert.Equal(t, int64(4), edge.Metric.ErrorCount)
	assert.InDelta(t, 12.5, edge.Metric.AvgLatency, 1e-9)
}

// TestSkyWalkingCollector_Collect_Error verifies that a non-2xx response is
// surfaced as an error containing the status code and body.
func TestSkyWalkingCollector_Collect_Error(t *testing.T) {
	srv := newSkyWalkingTestServer(http.StatusInternalServerError, []byte(`boom`))
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "500")
}

// TestSkyWalkingCollector_Collect_EmptyResponse verifies that a valid but
// empty GraphQL response yields a non-nil, empty Topology rather than an
// error.
func TestSkyWalkingCollector_Collect_EmptyResponse(t *testing.T) {
	resp := skywalkingGraphQLResponse{Data: skywalkingTopology{}}
	body, err := json.Marshal(resp)
	require.NoError(t, err)

	srv := newSkyWalkingTestServer(http.StatusOK, body)
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.NoError(t, err)
	require.NotNil(t, topo)
	assert.Empty(t, topo.Nodes)
	assert.Empty(t, topo.Edges)
}

// TestSkyWalkingCollector_Collect_GraphQLErrors verifies that GraphQL-level
// errors in the response envelope are surfaced as an error.
func TestSkyWalkingCollector_Collect_GraphQLErrors(t *testing.T) {
	resp := skywalkingGraphQLResponse{
		Errors: []skywalkingGQLError{{Message: "field 'foo' does not exist"}},
	}
	body, err := json.Marshal(resp)
	require.NoError(t, err)

	srv := newSkyWalkingTestServer(http.StatusOK, body)
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "graphql")
}

// TestSkyWalkingCollector_Collect_InvalidJSON verifies that a malformed body
// is reported as a decode error.
func TestSkyWalkingCollector_Collect_InvalidJSON(t *testing.T) {
	srv := newSkyWalkingTestServer(http.StatusOK, []byte(`{not json`))
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "decode")
}

// TestSkyWalkingCollector_Collect_CancelledContext verifies that the
// collector honours context cancellation before issuing the request.
func TestSkyWalkingCollector_Collect_CancelledContext(t *testing.T) {
	srv := newSkyWalkingTestServer(http.StatusOK, []byte(`{}`))
	defer srv.Close()

	c := NewSkyWalkingCollector(srv.URL+"/graphql", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}
	topo, err := c.Collect(ctx, tr)
	require.Error(t, err)
	assert.Nil(t, topo)
}

// TestSkyWalkingCollector_buildRequestPayload_Step verifies that the
// duration step switches to HOUR for windows of two hours or more.
func TestSkyWalkingCollector_buildRequestPayload_Step(t *testing.T) {
	c := NewSkyWalkingCollector("http://example/graphql", nil)

	short := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}
	body, err := c.buildRequestPayload(short)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"step":"MINUTE"`)

	long := TimeRange{Start: time.Now().Add(-3 * time.Hour), End: time.Now()}
	body, err = c.buildRequestPayload(long)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"step":"HOUR"`)
}
