package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- PreApplySLOGate: construction ----------------------------------------

func TestNewPreApplySLOGateDefaults(t *testing.T) {
	g := NewPreApplySLOGate("baseline", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	})
	assert.Equal(t, "baseline", g.Name())
	assert.Equal(t, PhasePreApply, g.Phase())
	assert.Equal(t, DefaultSLOSource, g.source)
	assert.Equal(t, DefaultSLOTimeout, g.timeout)
	assert.Equal(t, DefaultSLORetries, g.retries)
	assert.Equal(t, DefaultSLORetryDelay, g.retryDelay)
	assert.Equal(t, OnFailureBlock, g.onFailure)
}

func TestNewPreApplySLOGateWithOptions(t *testing.T) {
	g := NewPreApplySLOGate("baseline", []SLOQuerySpec{
		{Query: "q", Threshold: 1, Compare: "lt"},
	},
		WithPreApplySource("http://prom:9090"),
		WithPreApplyTimeout(7*time.Second),
		WithPreApplyRetries(3),
		WithPreApplyRetryDelay(250*time.Millisecond),
		WithPreApplyOnFailure(OnFailureWarn),
		WithPreApplyBaselineWindow(10*time.Minute),
	)
	assert.Equal(t, "http://prom:9090", g.source)
	assert.Equal(t, 7*time.Second, g.timeout)
	assert.Equal(t, 3, g.retries)
	assert.Equal(t, 250*time.Millisecond, g.retryDelay)
	assert.Equal(t, OnFailureWarn, g.onFailure)
	assert.Equal(t, 10*time.Minute, g.baselineWindow)
}

func TestPreApplySLOGateAlwaysPreApply(t *testing.T) {
	g := NewPreApplySLOGate("g", []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}})
	assert.Equal(t, PhasePreApply, g.Phase())
}

func TestPreApplySLOGateImplementsGateInterface(t *testing.T) {
	var _ Gate = (*PreApplySLOGate)(nil)
}

// --- PreApplySLOGate: single query success / failure ---------------------

func TestPreApplySLOGateSingleQueryPass(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt", Label: "error-rate"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(1), calls.Load())
	assert.Equal(t, "all_queries_within_threshold", r.Details["reason"])
	assert.Contains(t, r.Message, "1/1 queries within threshold")

	queries := r.Details["queries"].([]map[string]any)
	require.Len(t, queries, 1)
	assert.True(t, queries[0]["passed"].(bool))
	assert.Equal(t, 0.005, queries[0]["value"])
	assert.Equal(t, "error-rate", queries[0]["label"])
}

func TestPreApplySLOGateSingleQueryExceedsThresholdBlock(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	// applyPolicy overwrites "reason" with the policy outcome.
	assert.Equal(t, "blocked_by_policy", r.Details["reason"])
	assert.Equal(t, "block", r.Details["policy"])
}

func TestPreApplySLOGateQueryError(t *testing.T) {
	srv, calls := promServer(promErrorResponse("bad query"), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(1),
		WithPreApplyRetryDelay(2*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load(), "1 initial + 1 retry")
	assert.Equal(t, "blocked_by_policy", r.Details["reason"])
}

func TestPreApplySLOGateHTTPStatusError(t *testing.T) {
	srv, _ := promServer("internal error", http.StatusInternalServerError)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "status 500")
}

// --- PreApplySLOGate: retries --------------------------------------------

func TestPreApplySLOGateRetrySuccess(t *testing.T) {
	script := []scriptResp{
		{body: promVectorResponse(0.02), status: http.StatusOK},
		{body: promVectorResponse(0.005), status: http.StatusOK},
	}
	srv, calls := promScriptedServer(script)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(2),
		WithPreApplyRetryDelay(2*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load())
}

// --- PreApplySLOGate: on-failure policies --------------------------------

func TestPreApplySLOGateOnFailureWarn(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
		WithPreApplyOnFailure(OnFailureWarn),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed, "warn still fails the gate")
	assert.Equal(t, "warn_by_policy", r.Details["reason"])
	assert.Equal(t, "warn", r.Details["policy"])
	assert.Contains(t, r.Message, "WARNING")
}

func TestPreApplySLOGateOnFailureSkip(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
		WithPreApplyOnFailure(OnFailureSkip),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed, "skip policy should make the gate pass")
	assert.Equal(t, "skipped_by_policy", r.Details["reason"])
	assert.Equal(t, "skip", r.Details["policy"])
	assert.Contains(t, r.Message, "skipped by policy")
}

func TestPreApplySLOGateOnFailureBlockDefault(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	// No OnFailure option: should default to block.
	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, "blocked_by_policy", r.Details["reason"])
}

// --- PreApplySLOGate: MultiSLO -------------------------------------------

func TestPreApplySLOGateMultiSLOAllPass(t *testing.T) {
	// Single server returns a value that satisfies both queries.
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("multi", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt", Label: "error-rate"},
		{Query: "rate(latency_p99[5m])", Threshold: 0.5, Compare: "lt", Label: "p99-latency"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load(), "one HTTP call per query")
	assert.Contains(t, r.Message, "2/2 queries within threshold")

	queries := r.Details["queries"].([]map[string]any)
	require.Len(t, queries, 2)
	assert.True(t, queries[0]["passed"].(bool))
	assert.True(t, queries[1]["passed"].(bool))
}

func TestPreApplySLOGateMultiSLOOneFails(t *testing.T) {
	// First query passes (0.005 < 0.01), second fails (0.02 >= 0.01).
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if idx == 0 {
			_, _ = w.Write([]byte(promVectorResponse(0.005)))
		} else {
			_, _ = w.Write([]byte(promVectorResponse(0.02)))
		}
	}))
	defer srv.Close()

	g := NewPreApplySLOGate("multi", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt", Label: "error-rate"},
		{Query: "rate(other[5m])", Threshold: 0.01, Compare: "lt", Label: "other"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, "blocked_by_policy", r.Details["reason"])

	queries := r.Details["queries"].([]map[string]any)
	require.Len(t, queries, 2)
	assert.True(t, queries[0]["passed"].(bool), "first query should pass")
	assert.False(t, queries[1]["passed"].(bool), "second query should fail")
}

func TestPreApplySLOGateMultiSLOFirstFailsSkipsRemaining(t *testing.T) {
	// When the first query fails the gate still evaluates every query so
	// that the audit trail is complete; this is by design (the gate does
	// not short-circuit on the first failure). We assert that both queries
	// are evaluated.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.02)))
	}))
	defer srv.Close()

	g := NewPreApplySLOGate("multi", []SLOQuerySpec{
		{Query: "q1", Threshold: 0.01, Compare: "lt"},
		{Query: "q2", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load(), "both queries should be evaluated")
}

// --- PreApplySLOGate: empty configuration ---------------------------------

func TestPreApplySLOGateNoQueriesConfigured(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("empty", nil,
		WithPreApplySource(srv.URL),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err, "no queries configured is a configuration error")
	assert.False(t, r.Passed)
	assert.Equal(t, "no_queries_configured", r.Details["reason"])
}

// --- PreApplySLOGate: context cancellation -------------------------------

func TestPreApplySLOGateAlreadyCancelledContext(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(0), calls.Load(), "should not query when ctx already cancelled")
	assert.Contains(t, r.Message, "cancelled before run")
}

func TestPreApplySLOGateContextCancelledMidRun(t *testing.T) {
	// Server that sleeps so the per-query timeout does not fire but the
	// caller's context does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.005)))
	}))
	defer srv.Close()

	g := NewPreApplySLOGate("err-rate", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyTimeout(30*time.Second),
		WithPreApplyRetries(0),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
}

// --- PreApplySLOGate: baseline window audit ------------------------------

func TestPreApplySLOGateBaselineWindowRecorded(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewPreApplySLOGate("baseline", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
		WithPreApplyBaselineWindow(15*time.Minute),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, (15 * time.Minute).String(), r.Details["baseline_window"])
}

// --- PreApplySLOGate: integration with GateManager -----------------------

func TestPreApplySLOGateRegisteredWithManager(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewPreApplySLOGate("baseline", []SLOQuerySpec{
		{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	))

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{RunID: "r1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}

func TestPreApplySLOGateManagerFailureTriggersSkip(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewPreApplySLOGate("a-fail", []SLOQuerySpec{
		{Query: "q1", Threshold: 0.01, Compare: "lt"},
	},
		WithPreApplySource(srv.URL),
		WithPreApplyRetries(0),
	))
	// slow-pass sorts after a-fail; it should be marked skipped.
	gm.Register(&mockGate{name: "b-slow", phase: PhasePreApply, pass: true, delay: 100 * time.Millisecond})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 2)
	assert.False(t, results[0].Passed)
	assert.False(t, isSkipped(results[0]), "a-fail should be a real failure")
	assert.True(t, isSkipped(results[1]), "b-slow should be skipped")
}
