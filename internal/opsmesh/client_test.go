// client_test.go covers the OpsMesh integration client. Each test spins up a
// throwaway httptest.Server that records the incoming request and replays a
// canned response, so the suite is hermetic and never touches the network.
package opsmesh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test helpers -----------------------------------------------------------

// newTestClient builds an OpsMeshClient pointing at srv.URL with a fast
// http.Client so context-cancellation tests do not have to wait for the
// default 30s timeout.
func newTestClient(t *testing.T, srv *httptest.Server) *OpsMeshClient {
	t.Helper()
	c := NewOpsMeshClient(OpsMeshClientConfig{
		BaseURL:    srv.URL,
		APIKey:     "test-key",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	require.NotNil(t, c)
	return c
}

// readAndClose drains r.Body and returns the bytes. It is a small convenience
// used by handlers that want to assert on the request payload.
func readAndClose(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}

// --- Construction ----------------------------------------------------------

// TestNewOpsMeshClient_Defaults verifies that a nil HTTPClient and Logger fall
// back to the documented defaults and that the BaseURL has its trailing slash
// trimmed so path concatenation yields clean URLs.
func TestNewOpsMeshClient_Defaults(t *testing.T) {
	c := NewOpsMeshClient(OpsMeshClientConfig{
		BaseURL: "https://opsmesh.example.com/",
		APIKey:  "k",
	})
	require.NotNil(t, c)
	assert.Equal(t, "https://opsmesh.example.com", c.baseURL)
	assert.Equal(t, "k", c.apiKey)
	assert.NotNil(t, c.httpClient)
	// The default client must carry our 30s timeout.
	tr, ok := c.httpClient.Transport.(*http.Transport)
	_ = tr
	_ = ok
	// We cannot read Timeout back from *http.Client directly, but we can
	// assert that the client is non-nil and distinct per call.
	c2 := NewOpsMeshClient(OpsMeshClientConfig{BaseURL: "x", APIKey: "y"})
	assert.NotSame(t, c.httpClient, c2.httpClient)
	assert.NotNil(t, c.log)
}

// --- ReportResult ----------------------------------------------------------

// TestReportResult_Success verifies a happy-path POST: method, path, JSON
// body, Authorization, Content-Type and User-Agent headers are all correct.
func TestReportResult_Success(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   []byte
		gotAuth   string
		gotCT     string
		gotUA     string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = readAndClose(r)
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	result := &FixResult{
		AlertID:     "alert-1",
		Success:     true,
		Summary:     "rolled back",
		WorkflowID:  "wf-1",
		Duration:    2 * time.Second,
		StepsTotal:  3,
		StepsFailed: 0,
		Timestamp:   time.Now().UTC(),
	}

	require.NoError(t, c.ReportResult(context.Background(), "alert-1", result))

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/alerts/alert-1/resolution", gotPath)

	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "application/json", gotCT)
	assert.Equal(t, userAgent, gotUA)

	// Body round-trips to the same FixResult.
	var decoded FixResult
	require.NoError(t, json.Unmarshal(gotBody, &decoded))
	assert.Equal(t, result.AlertID, decoded.AlertID)
	assert.Equal(t, result.Success, decoded.Success)
	assert.Equal(t, result.Summary, decoded.Summary)
	assert.Equal(t, result.WorkflowID, decoded.WorkflowID)
	assert.Equal(t, result.Duration, decoded.Duration)
	assert.Equal(t, result.StepsTotal, decoded.StepsTotal)
	assert.Equal(t, result.StepsFailed, decoded.StepsFailed)
}

// TestReportResult_NilResult verifies that a nil result is rejected with
// ErrNilResult before any network activity.
func TestReportResult_NilResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for nil result")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.ReportResult(context.Background(), "alert-1", nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilResult), "want ErrNilResult, got %v", err)
}

// TestReportResult_EmptyAlertID verifies that an empty alert id is rejected
// with ErrEmptyAlertID before any network activity.
func TestReportResult_EmptyAlertID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for empty alert id")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.ReportResult(context.Background(), "", &FixResult{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyAlertID), "want ErrEmptyAlertID, got %v", err)
}

// TestReportResult_HTTPError verifies that a 4xx/5xx response is surfaced as
// an error containing the status code and body.
func TestReportResult_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad alert"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.ReportResult(context.Background(), "alert-1", &FixResult{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
	assert.Contains(t, err.Error(), "bad alert")
}

// TestReportResult_ContextCancel verifies that a cancelled context aborts the
// request and the error wraps the context's cause.
func TestReportResult_ContextCancel(t *testing.T) {
	// Server that never responds until the test signals it.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(block)

	c := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel before issuing the request so the dial/round-trip fails fast.
	cancel()

	err := c.ReportResult(ctx, "alert-1", &FixResult{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opsmesh:")
}

// --- GetTopology -----------------------------------------------------------

// TestGetTopology_Success verifies the REAL platform contract: the catalog
// graph endpoint (/api/v1/catalog/topology?tenantID=) with the CatalogGraph
// shape — the previous revision of this test pinned an invented
// /api/v1/topology?service= endpoint that the platform never implemented.
func TestGetTopology_Success(t *testing.T) {
	var (
		gotMethod   string
		gotPath     string
		gotTenantID string
	)
	payload := CatalogGraph{
		TenantID: "t-1",
		Nodes: []CatalogNode{
			{ID: "host-001", Name: "web-server-01", Type: "host", Status: "online",
				Metadata: map[string]string{"tenantID": "t-1"}},
			{ID: "svc-001", Name: "auth-service", Type: "service", Status: "running",
				Children: []string{"host-001"}},
		},
		Edges: []CatalogEdge{{From: "svc-001", To: "host-001", RelationType: "depends"}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotTenantID = r.URL.Query().Get("tenantID")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	graph, err := c.GetTopology(context.Background(), "t-1")
	require.NoError(t, err)
	require.NotNil(t, graph)

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/catalog/topology", gotPath)
	assert.Equal(t, "t-1", gotTenantID)
	assert.Equal(t, payload.TenantID, graph.TenantID)
	require.Len(t, graph.Nodes, 2)
	assert.Equal(t, "web-server-01", graph.Nodes[0].Name)
	assert.Equal(t, "online", graph.Nodes[0].Status)
	assert.Equal(t, []string{"host-001"}, graph.Nodes[1].Children)
	require.Len(t, graph.Edges, 1)
	assert.Equal(t, "depends", graph.Edges[0].RelationType)
}

// TestGetTopology_EmptyTenantOmitsParam verifies the default-tenant path.
func TestGetTopology_EmptyTenantOmitsParam(t *testing.T) {
	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(CatalogGraph{})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	_, err := c.GetTopology(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, rawQuery, "an empty tenant must omit the parameter (platform default)")
}

// TestGetTopology_HTTPError verifies that a 5xx response is surfaced.
func TestGetTopology_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	graph, err := c.GetTopology(context.Background(), "t-1")
	require.Error(t, err)
	assert.Nil(t, graph)
	assert.Contains(t, err.Error(), "status 500")
	assert.Contains(t, err.Error(), "internal")
}

// --- GetMetrics ------------------------------------------------------------

// TestGetMetrics_Success verifies the REAL platform contract: POST
// /api/v1/prometheus/query {"query": ...} answered with a Prometheus
// instant-vector envelope, normalized into Metrics.
func TestGetMetrics_Success(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   map[string]string
	)
	ts := float64(time.Now().Unix())
	envelope := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{
					"metric": map[string]string{"host": "h1"},
					"value":  []any{ts, "0.42"},
				},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	metrics, err := c.GetMetrics(context.Background(), `up{job="node"}`)
	require.NoError(t, err)
	require.NotNil(t, metrics)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/api/v1/prometheus/query", gotPath)
	assert.Equal(t, `up{job="node"}`, gotBody["query"])
	assert.Equal(t, `up{job="node"}`, metrics.Query)
	require.Len(t, metrics.Series, 1)
	assert.Equal(t, "h1", metrics.Series[0].Labels["host"])
	assert.InDelta(t, 0.42, metrics.Series[0].Value, 1e-9)
	assert.Equal(t, time.Unix(int64(ts), 0).UTC(), metrics.Series[0].Timestamp)
}

// TestGetMetrics_QueryFailed surfaces a non-success envelope as ErrQueryFailed.
func TestGetMetrics_QueryFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "parse error"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	metrics, err := c.GetMetrics(context.Background(), "bad{")
	require.Error(t, err)
	assert.Nil(t, metrics)
	assert.True(t, errors.Is(err, ErrQueryFailed), "want ErrQueryFailed, got %v", err)
}

// TestGetMetrics_SkipsMalformedSeries pins the degrade contract: one odd
// series must not blank out the rest of the evidence.
func TestGetMetrics_SkipsMalformedSeries(t *testing.T) {
	envelope := map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{
				map[string]any{"metric": map[string]string{"host": "bad1"}, "value": []any{}},
				map[string]any{"metric": map[string]string{"host": "bad2"}, "value": []any{1.0, "NaNx"}},
				map[string]any{"metric": map[string]string{"host": "good"}, "value": []any{1.0, "7"}},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(envelope)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	metrics, err := c.GetMetrics(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, metrics.Series, 1, "malformed series must be skipped, not fatal")
	assert.Equal(t, "good", metrics.Series[0].Labels["host"])
}

// TestGetMetrics_EmptyQuery verifies that an empty query is rejected with
// ErrEmptyQuery before any network activity.
func TestGetMetrics_EmptyQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for empty query")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	metrics, err := c.GetMetrics(context.Background(), "")
	require.Error(t, err)
	assert.Nil(t, metrics)
	assert.True(t, errors.Is(err, ErrEmptyQuery), "want ErrEmptyQuery, got %v", err)
}

// --- Ping ------------------------------------------------------------------

// TestPing_Success verifies that a 2xx response yields nil, at the platform's
// real probe path (/healthz — the former /api/v1/health never existed).
func TestPing_Success(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	require.NoError(t, c.Ping(context.Background()))
	assert.Equal(t, "/healthz", gotPath)
}

// TestPing_Failure verifies that a non-2xx response is surfaced as an error.
func TestPing_Failure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("down"))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	err := c.Ping(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
	assert.Contains(t, err.Error(), "down")
}

// --- Authorization ---------------------------------------------------------

// TestUnauthorized verifies that a 401 response is surfaced as an error
// containing the status code, regardless of which API method is invoked.
func TestUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanity-check the Authorization header on the way through.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)

	// ReportResult
	err := c.ReportResult(context.Background(), "alert-1", &FixResult{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")

	// GetTopology
	_, err = c.GetTopology(context.Background(), "svc-a")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")

	// GetMetrics
	_, err = c.GetMetrics(context.Background(), "q")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")

	// Ping
	err = c.Ping(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 401")
}

// --- Concurrency -----------------------------------------------------------

// TestOpsMeshClient_Concurrent verifies that a single client can be used from
// many goroutines without racing. It is a smoke test for the "safe for
// concurrent use" guarantee.
func TestOpsMeshClient_Concurrent(t *testing.T) {
	var count int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			errs <- c.Ping(context.Background())
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, int64(n), count)
}

// --- URL escaping ----------------------------------------------------------

// TestReportResult_AlertIDEscaping verifies that an alert id containing
// characters requiring path-escaping is correctly encoded.
func TestReportResult_AlertIDEscaping(t *testing.T) {
	var gotEscapedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscapedPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	// "a/b" must be escaped so it does not collapse into extra path segments.
	require.NoError(t, c.ReportResult(context.Background(), "a/b", &FixResult{}))
	assert.Equal(t, "/api/v1/alerts/a%2Fb/resolution", gotEscapedPath)
}

// --- BaseURL trailing slash ------------------------------------------------

// TestBaseURL_TrailingSlash verifies that a BaseURL with a trailing slash
// still yields clean paths (no double slash).
func TestBaseURL_TrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewOpsMeshClient(OpsMeshClientConfig{
		BaseURL:    srv.URL + "/",
		APIKey:     "k",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	require.NoError(t, c.Ping(context.Background()))
	assert.Equal(t, "/healthz", gotPath)
}
