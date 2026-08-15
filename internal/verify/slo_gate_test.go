package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Prometheus mock helpers ----------------------------------------------
//
// These helpers build httptest.Server instances that return scripted
// Prometheus responses so that SLOGate tests can exercise the full HTTP
// path without a real Prometheus instance.

// promVectorResponse builds a Prometheus instant-query response body for a
// vector result with a single sample of the given value.
func promVectorResponse(value float64) string {
	type sample struct {
		Metric map[string]string `json:"metric"`
		Value  [2]any            `json:"value"`
	}
	type data struct {
		ResultType string   `json:"resultType"`
		Result     []sample `json:"result"`
	}
	type resp struct {
		Status string `json:"status"`
		Data   data   `json:"data"`
	}
	r := resp{
		Status: "success",
		Data: data{
			ResultType: "vector",
			Result: []sample{
				{Metric: map[string]string{}, Value: [2]any{time.Now().Unix(), fmt.Sprintf("%g", value)}},
			},
		},
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// promScalarResponse builds a Prometheus response for a scalar result.
func promScalarResponse(value float64) string {
	type data struct {
		ResultType string `json:"resultType"`
		Result     [2]any `json:"result"`
	}
	type resp struct {
		Status string `json:"status"`
		Data   data   `json:"data"`
	}
	r := resp{
		Status: "success",
		Data: data{
			ResultType: "scalar",
			Result:     [2]any{time.Now().Unix(), fmt.Sprintf("%g", value)},
		},
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// promErrorResponse builds a Prometheus response with status "error".
func promErrorResponse(errMsg string) string {
	type resp struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
	}
	r := resp{Status: "error", ErrorType: "bad_data", Error: errMsg}
	b, _ := json.Marshal(r)
	return string(b)
}

// promServer returns an httptest.Server that responds to /api/v1/query with
// the given body and status. The handler also counts requests atomically.
func promServer(body string, status int) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return srv, &calls
}

// promScriptedServer returns an httptest.Server that responds to each request
// with the next entry in the script. Once the script is exhausted the last
// entry is repeated.
type scriptResp struct {
	body   string
	status int
}

func promScriptedServer(script []scriptResp) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		var entry scriptResp
		if idx < len(script) {
			entry = script[idx]
		} else if len(script) > 0 {
			entry = script[len(script)-1]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(entry.status)
		_, _ = w.Write([]byte(entry.body))
	}))
	return srv, &calls
}

// --- tests: construction ---------------------------------------------------

func TestNewSLOGateDefaults(t *testing.T) {
	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt")
	assert.Equal(t, "err-rate", g.Name())
	assert.Equal(t, PhasePostBatch, g.Phase())
	assert.Equal(t, "rate(errors[5m])", g.query)
	assert.Equal(t, 0.01, g.threshold)
	assert.Equal(t, CompareLT, g.compare)
	assert.Equal(t, DefaultSLOSource, g.source)
	assert.Equal(t, DefaultSLOTimeout, g.timeout)
	assert.Equal(t, DefaultSLORetries, g.retries)
	assert.Equal(t, DefaultSLORetryDelay, g.retryDelay)
}

func TestNewSLOGateWithOptions(t *testing.T) {
	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "gt",
		WithSLOSource("http://prom:9090"),
		WithSLOTimeout(5*time.Second),
		WithSLORetries(4),
		WithSLORetryDelay(500*time.Millisecond),
	)
	assert.Equal(t, "http://prom:9090", g.source)
	assert.Equal(t, 5*time.Second, g.timeout)
	assert.Equal(t, 4, g.retries)
	assert.Equal(t, 500*time.Millisecond, g.retryDelay)
	assert.Equal(t, CompareGT, g.compare)
}

func TestNewSLOGateUnknownCompareDefaultsToLT(t *testing.T) {
	g := NewSLOGate("g", "q", 1.0, "bogus")
	assert.Equal(t, CompareLT, g.compare, "unknown compare should default to lt")
}

func TestSLOGateAlwaysPostBatch(t *testing.T) {
	g := NewSLOGate("g", "q", 1.0, "lt")
	assert.Equal(t, PhasePostBatch, g.Phase())
}

// --- tests: success / threshold exceeded ----------------------------------

func TestSLOGateWithinThreshold(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(1), calls.Load())
	assert.Equal(t, 0.005, r.Details["value"])
}

func TestSLOGateExceedsThreshold(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "threshold")
	assert.Equal(t, "threshold_exceeded", r.Details["reason"])
	assert.Equal(t, int64(1), calls.Load())
}

func TestSLOGateScalarResult(t *testing.T) {
	srv, _ := promServer(promScalarResponse(0.003), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("scalar", "1 - vector(1)", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, 0.003, r.Details["value"])
}

// --- tests: comparison operators ------------------------------------------

func TestSLOGateCompareOperators(t *testing.T) {
	cases := []struct {
		compare   string
		value     float64
		threshold float64
		pass      bool
	}{
		{"lt", 0.005, 0.01, true},
		{"lt", 0.01, 0.01, false},
		{"lt", 0.02, 0.01, false},
		{"le", 0.01, 0.01, true},
		{"le", 0.02, 0.01, false},
		{"gt", 0.02, 0.01, true},
		{"gt", 0.01, 0.01, false},
		{"ge", 0.01, 0.01, true},
		{"ge", 0.005, 0.01, false},
		{"eq", 0.01, 0.01, true},
		{"eq", 0.02, 0.01, false},
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("%d_%s_value_%g_thresh_%g", i, c.compare, c.value, c.threshold), func(t *testing.T) {
			srv, _ := promServer(promVectorResponse(c.value), http.StatusOK)
			defer srv.Close()

			g := NewSLOGate("op", "q", c.threshold, c.compare,
				WithSLOSource(srv.URL),
				WithSLORetries(0),
			)
			r, err := g.Check(context.Background(), GateInput{})
			require.NoError(t, err)
			assert.Equal(t, c.pass, r.Passed, "compare %s value %g threshold %g", c.compare, c.value, c.threshold)
		})
	}
}

// --- tests: query failure / retries ---------------------------------------

func TestSLOGateQueryErrorAllRetries(t *testing.T) {
	srv, calls := promServer(promErrorResponse("bad query"), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(2),
		WithSLORetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(3), calls.Load(), "1 initial + 2 retries")
	assert.Contains(t, r.Message, "query failed")
}

func TestSLOGateHTTPStatusError(t *testing.T) {
	srv, _ := promServer("internal error", http.StatusInternalServerError)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "status 500")
}

func TestSLOGateRetrySuccess(t *testing.T) {
	// First attempt exceeds threshold, second is within.
	script := []scriptResp{
		{body: promVectorResponse(0.02), status: http.StatusOK},
		{body: promVectorResponse(0.005), status: http.StatusOK},
	}
	srv, calls := promScriptedServer(script)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(2),
		WithSLORetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed, "should pass on second attempt")
	assert.Equal(t, int64(2), calls.Load())
}

func TestSLOGateRetryFailureAllAttempts(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(2),
		WithSLORetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(3), calls.Load())
}

func TestSLOGateRetryOnErrorThenSuccess(t *testing.T) {
	script := []scriptResp{
		{body: promErrorResponse("transient"), status: http.StatusOK},
		{body: promVectorResponse(0.005), status: http.StatusOK},
	}
	srv, calls := promScriptedServer(script)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(2),
		WithSLORetryDelay(5*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load())
}

// --- tests: timeout -------------------------------------------------------

func TestSLOGateTimeout(t *testing.T) {
	// Server that sleeps 200ms before responding; gate timeout is 20ms.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.005)))
	}))
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLOTimeout(20*time.Millisecond),
		WithSLORetries(0),
	)

	start := time.Now()
	r, err := g.Check(context.Background(), GateInput{})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Less(t, elapsed, 150*time.Millisecond, "should not wait for full server delay")
}

func TestSLOGateCallerContextTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.005)))
	}))
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLOTimeout(30*time.Second),
		WithSLORetries(0),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
}

func TestSLOGateAlreadyCancelledContext(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(0), calls.Load(), "should not query when ctx already cancelled")
	assert.Contains(t, r.Message, "cancelled before run")
}

// --- tests: edge cases ----------------------------------------------------

func TestSLOGateEmptyVector(t *testing.T) {
	// Prometheus can return an empty vector when there are no samples.
	body := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	srv, _ := promServer(body, http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "empty vector")
}

func TestSLOGateMalformedJSON(t *testing.T) {
	srv, _ := promServer("not json", http.StatusOK)
	defer srv.Close()

	g := NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "decode response")
}

// --- tests: gate interface ------------------------------------------------

func TestSLOGateImplementsGateInterface(t *testing.T) {
	var _ Gate = (*SLOGate)(nil)

	g := NewSLOGate("err-rate", "q", 0.01, "lt")
	assert.Equal(t, "err-rate", g.Name())
	assert.Equal(t, PhasePostBatch, g.Phase())
}

// --- tests: integration with GateManager ----------------------------------

func TestSLOGateRegisteredWithManager(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewSLOGate("err-rate", "rate(errors[5m])", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{BatchID: "b1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}

func TestSLOGateManagerFailureTriggersSkip(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewSLOGate("a-fail", "q1", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	))
	gm.Register(NewSLOGate("b-fail", "q2", 0.01, "lt",
		WithSLOSource(srv.URL),
		WithSLORetries(0),
	))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{})
	require.Len(t, results, 2)
	// Both gates fail; at least one is a real failure.
	failedCount := 0
	for _, r := range results {
		if !r.Passed && !isSkipped(r) {
			failedCount++
		}
	}
	assert.GreaterOrEqual(t, failedCount, 1)
}
