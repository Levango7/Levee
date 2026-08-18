package topology

// pinpoint_test.go exercises the PinpointCollector against an httptest mock
// server. It covers the happy path (server list + server map projected into
// the unified Topology), the error path (HTTP 500 on either endpoint), the
// empty-response path and the collector Name accessor.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinpointTestServer dispatches /getServerList and /getServerMapDataV2 to
// caller-supplied handlers so a single test can vary both responses.
type pinpointTestServer struct {
	*httptest.Server
	listHandler func(w http.ResponseWriter, r *http.Request)
	mapHandler  func(w http.ResponseWriter, r *http.Request)
}

func newPinpointTestServer(listStatus, mapStatus int, listBody, mapBody []byte) *pinpointTestServer {
	s := &pinpointTestServer{
		listHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(listStatus)
			_, _ = w.Write(listBody)
		},
		mapHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(mapStatus)
			_, _ = w.Write(mapBody)
		},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getServerList"):
			s.listHandler(w, r)
		case strings.HasSuffix(r.URL.Path, "/getServerMapDataV2"):
			s.mapHandler(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return s
}

// TestPinpointCollector_Name verifies the stable collector identifier.
func TestPinpointCollector_Name(t *testing.T) {
	c := NewPinpointCollector("http://example", nil)
	assert.Equal(t, "pinpoint", c.Name())
}

// TestPinpointCollector_Collect_Success mocks both endpoints and asserts
// that the collector projects the merged payload into the unified Topology,
// including the microsecond→millisecond latency conversion.
func TestPinpointCollector_Collect_Success(t *testing.T) {
	list := pinpointServerList{
		ServerList: []pinpointServer{
			{ID: "10", Name: "web", Type: "HTTP", Endpoint: "web:80", Metadata: map[string]string{"agent": "pinpoint"}},
			{ID: "20", Name: "cart", Type: "RPC", Endpoint: "cart:8080"},
		},
	}
	listBody, err := json.Marshal(list)
	require.NoError(t, err)

	m := pinpointServerMap{
		LinkList: []pinpointLink{
			{Source: "10", Target: "20", CallCount: 50, ErrorCount: 2, AvgLatency: 7300}, // 7.3 ms
		},
	}
	mapBody, err := json.Marshal(m)
	require.NoError(t, err)

	srv := newPinpointTestServer(http.StatusOK, http.StatusOK, listBody, mapBody)
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	tr := TimeRange{Start: time.Now().Add(-time.Hour), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.NoError(t, err)
	require.NotNil(t, topo)

	require.Len(t, topo.Nodes, 2)
	assert.Equal(t, "web", topo.Nodes[0].Name)
	assert.Equal(t, "pinpoint", topo.Nodes[0].Metadata["agent"])
	assert.Equal(t, "cart", topo.Nodes[1].Name)

	require.Len(t, topo.Edges, 1)
	edge := topo.Edges[0]
	assert.Equal(t, "10", edge.Source)
	assert.Equal(t, "20", edge.Target)
	assert.Equal(t, int64(50), edge.Metric.CallCount)
	assert.Equal(t, int64(2), edge.Metric.ErrorCount)
	assert.InDelta(t, 7.3, edge.Metric.AvgLatency, 1e-9)
}

// TestPinpointCollector_Collect_Error verifies that a 500 on the server-list
// endpoint is surfaced as an error.
func TestPinpointCollector_Collect_Error(t *testing.T) {
	srv := newPinpointTestServer(
		http.StatusInternalServerError, http.StatusOK,
		[]byte(`list boom`), []byte(`{}`),
	)
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "server list")
}

// TestPinpointCollector_Collect_ErrorServerMap verifies that a 500 on the
// server-map endpoint is surfaced as an error even when the server list
// succeeded.
func TestPinpointCollector_Collect_ErrorServerMap(t *testing.T) {
	listBody, err := json.Marshal(pinpointServerList{})
	require.NoError(t, err)

	srv := newPinpointTestServer(
		http.StatusOK, http.StatusInternalServerError,
		listBody, []byte(`map boom`),
	)
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "server map")
}

// TestPinpointCollector_Collect_EmptyResponse verifies that valid but empty
// payloads yield a non-nil, empty Topology.
func TestPinpointCollector_Collect_EmptyResponse(t *testing.T) {
	listBody, err := json.Marshal(pinpointServerList{})
	require.NoError(t, err)
	mapBody, err := json.Marshal(pinpointServerMap{})
	require.NoError(t, err)

	srv := newPinpointTestServer(http.StatusOK, http.StatusOK, listBody, mapBody)
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.NoError(t, err)
	require.NotNil(t, topo)
	assert.Empty(t, topo.Nodes)
	assert.Empty(t, topo.Edges)
}

// TestPinpointCollector_Collect_InvalidJSON verifies that a malformed
// server-list body is reported as a decode error.
func TestPinpointCollector_Collect_InvalidJSON(t *testing.T) {
	srv := newPinpointTestServer(
		http.StatusOK, http.StatusOK,
		[]byte(`{not json`), []byte(`{}`),
	)
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}

	topo, err := c.Collect(context.Background(), tr)
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "decode")
}

// TestPinpointCollector_Collect_CancelledContext verifies that the collector
// honours context cancellation before issuing the request.
func TestPinpointCollector_Collect_CancelledContext(t *testing.T) {
	srv := newPinpointTestServer(http.StatusOK, http.StatusOK, []byte(`{}`), []byte(`{}`))
	defer srv.Close()

	c := NewPinpointCollector(srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tr := TimeRange{Start: time.Now().Add(-time.Minute), End: time.Now()}
	topo, err := c.Collect(ctx, tr)
	require.Error(t, err)
	assert.Nil(t, topo)
}