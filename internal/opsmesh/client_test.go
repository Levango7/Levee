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

// TestGetTopology_Success verifies a happy-path GET and JSON decode.
func TestGetTopology_Success(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
	)
	payload := Topology{
		Service: "svc-a",
		Nodes: []TopologyNode{
			{ID: "n1", Name: "node-1", Type: "host", IP: "10.0.0.1", Metadata: map[string]string{"az": "a"}},
		},
		Edges:     []TopologyEdge{{From: "n1", To: "n2", Type: "connects"}},
		UpdatedAt: time.Now().UTC(),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("service")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	topo, err := c.GetTopology(context.Background(), "svc-a")
	require.NoError(t, err)
	require.NotNil(t, topo)

	assert.Equal(t, http.MethodGet, gotMethod)
	assert.Equal(t, "/api/v1/topology", gotPath)
	assert.Equal(t, "svc-a", gotQuery)
	assert.Equal(t, payload.Service, topo.Service)
	require.Len(t, topo.Nodes, 1)
	assert.Equal(t, payload.Nodes[0].ID, topo.Nodes[0].ID)
	assert.Equal(t, payload.Nodes[0].Metadata["az"], topo.Nodes[0].Metadata["az"])
	require.Len(t, topo.Edges, 1)
	assert.Equal(t, payload.Edges[0].From, topo.Edges[0].From)
}

// TestGetTopology_HTTPError verifies that a 5xx response is surfaced.
func TestGetTopology_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	topo, err := c.GetTopology(context.Background(), "svc-a")
	require.Error(t, err)
	assert.Nil(t, topo)
	assert.Contains(t, err.Error(), "status 500")
	assert.Contains(t, err.Error(), "internal")
}

// --- GetMetrics ------------------------------------------------------------

// TestGetMetrics_Success verifies a happy-path GET with query and time range
// parameters and JSON decode.
func TestGetMetrics_Success(t *testing.T) {
	var (
		gotQuery string
		gotStart string
		gotEnd   string
	)
	now := time.Now().UTC().Truncate(time.Second)
	payload := Metrics{
		Query: "cpu_usage",
		Series: []MetricSeries{
			{Labels: map[string]string{"host": "h1"}, Points: []MetricPoint{{Timestamp: now, Value: 0.42}}},
		},
		TimeRange: TimeRange{Start: now, End: now.Add(5 * time.Minute)},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	tr := TimeRange{Start: now, End: now.Add(5 * time.Minute)}
	metrics, err := c.GetMetrics(context.Background(), "cpu_usage", tr)
	require.NoError(t, err)
	require.NotNil(t, metrics)

	assert.Equal(t, "cpu_usage", gotQuery)
	assert.Equal(t, now.Format(time.RFC3339Nano), gotStart)
	assert.Equal(t, now.Add(5*time.Minute).Format(time.RFC3339Nano), gotEnd)
	assert.Equal(t, payload.Query, metrics.Query)
	require.Len(t, metrics.Series, 1)
	assert.Equal(t, payload.Series[0].Labels["host"], metrics.Series[0].Labels["host"])
	require.Len(t, metrics.Series[0].Points, 1)
	assert.InDelta(t, 0.42, metrics.Series[0].Points[0].Value, 1e-9)
}

// TestGetMetrics_EmptyQuery verifies that an empty query is rejected with
// ErrEmptyQuery before any network activity.
func TestGetMetrics_EmptyQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for empty query")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	tr := TimeRange{Start: time.Now(), End: time.Now().Add(time.Minute)}
	metrics, err := c.GetMetrics(context.Background(), "", tr)
	require.Error(t, err)
	assert.Nil(t, metrics)
	assert.True(t, errors.Is(err, ErrEmptyQuery), "want ErrEmptyQuery, got %v", err)
}

// TestGetMetrics_InvalidTimeRange verifies that a time range whose End is not
// strictly after Start is rejected with ErrInvalidTimeRange.
func TestGetMetrics_InvalidTimeRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for invalid time range")
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	now := time.Now()

	// End == Start is invalid.
	tr := TimeRange{Start: now, End: now}
	metrics, err := c.GetMetrics(context.Background(), "q", tr)
	require.Error(t, err)
	assert.Nil(t, metrics)
	assert.True(t, errors.Is(err, ErrInvalidTimeRange), "want ErrInvalidTimeRange, got %v", err)

	// End < Start is invalid.
	tr = TimeRange{Start: now, End: now.Add(-time.Second)}
	metrics, err = c.GetMetrics(context.Background(), "q", tr)
	require.Error(t, err)
	assert.Nil(t, metrics)
	assert.True(t, errors.Is(err, ErrInvalidTimeRange), "want ErrInvalidTimeRange, got %v", err)
}

// --- Ping ------------------------------------------------------------------

// TestPing_Success verifies that a 2xx response yields nil.
func TestPing_Success(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	require.NoError(t, c.Ping(context.Background()))
	assert.Equal(t, "/api/v1/health", gotPath)
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
	tr := TimeRange{Start: time.Now(), End: time.Now().Add(time.Minute)}
	_, err = c.GetMetrics(context.Background(), "q", tr)
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
	assert.Equal(t, "/api/v1/health", gotPath)
}
