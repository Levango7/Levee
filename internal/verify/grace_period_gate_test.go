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

// --- GracePeriodGate: construction ----------------------------------------

func TestNewGracePeriodGateDefaults(t *testing.T) {
	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 5 * time.Minute,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	})
	assert.Equal(t, "cool-down", g.Name())
	assert.Equal(t, PhaseGracePeriod, g.Phase())
	assert.Equal(t, DefaultSLOSource, g.source)
	assert.Equal(t, DefaultSLOTimeout, g.timeout)
	assert.Equal(t, DefaultSLORetries, g.retries)
	assert.Equal(t, DefaultSLORetryDelay, g.retryDelay)
	assert.Equal(t, OnFailureBlock, g.onFailure)
	assert.Equal(t, 5*time.Minute, g.config.Duration)
}

func TestNewGracePeriodGateWithOptions(t *testing.T) {
	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 3 * time.Minute,
		SLOQueries: []SLOQuerySpec{
			{Query: "q", Threshold: 1, Compare: "lt"},
		},
		OnFailure: "warn",
	},
		WithGracePeriodSource("http://prom:9090"),
		WithGracePeriodTimeout(8*time.Second),
		WithGracePeriodRetries(3),
		WithGracePeriodRetryDelay(300*time.Millisecond),
		WithGracePeriodOnFailure(OnFailureSkip),
	)
	assert.Equal(t, "http://prom:9090", g.source)
	assert.Equal(t, 8*time.Second, g.timeout)
	assert.Equal(t, 3, g.retries)
	assert.Equal(t, 300*time.Millisecond, g.retryDelay)
	assert.Equal(t, OnFailureSkip, g.onFailure, "explicit option overrides config string")
	assert.Equal(t, 3*time.Minute, g.config.Duration)
}

func TestNewGracePeriodGateOnFailureFromConfig(t *testing.T) {
	g := NewGracePeriodGate("g", GracePeriodConfig{
		Duration:   time.Minute,
		SLOQueries: []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}},
		OnFailure:  "warn",
	})
	assert.Equal(t, OnFailureWarn, g.onFailure)
}

func TestNewGracePeriodGateDurationClampedToMax(t *testing.T) {
	g := NewGracePeriodGate("g", GracePeriodConfig{
		Duration:   2 * time.Hour, // well above MaxGracePeriodDuration
		SLOQueries: []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}},
	})
	assert.Equal(t, MaxGracePeriodDuration, g.config.Duration, "duration should be clamped to max")
}

func TestNewGracePeriodGateNegativeDurationTreatedAsZero(t *testing.T) {
	g := NewGracePeriodGate("g", GracePeriodConfig{
		Duration:   -1 * time.Minute,
		SLOQueries: []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}},
	})
	assert.Equal(t, time.Duration(0), g.config.Duration, "negative duration should be treated as 0")
}

func TestGracePeriodGateAlwaysGracePeriodPhase(t *testing.T) {
	g := NewGracePeriodGate("g", GracePeriodConfig{
		Duration:   time.Minute,
		SLOQueries: []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}},
	})
	assert.Equal(t, PhaseGracePeriod, g.Phase())
}

func TestGracePeriodGateImplementsGateInterface(t *testing.T) {
	var _ Gate = (*GracePeriodGate)(nil)
}

// --- GracePeriodGate: duration == 0 short-circuit ------------------------

func TestGracePeriodGateDurationZeroSkips(t *testing.T) {
	// Server that would fail every request; we want to confirm the gate
	// never queries it.
	srv, calls := promServer(promErrorResponse("should not be called"), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("no-wait", GracePeriodConfig{
		Duration:   0,
		SLOQueries: []SLOQuerySpec{{Query: "q", Threshold: 1, Compare: "lt"}},
	},
		WithGracePeriodSource(srv.URL),
	)

	start := time.Now()
	r, err := g.Check(context.Background(), GateInput{})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.True(t, r.Passed, "duration=0 should short-circuit to a pass")
	assert.Equal(t, int64(0), calls.Load(), "no query should be issued")
	assert.Equal(t, "duration_zero_skipped", r.Details["reason"])
	assert.Less(t, elapsed, 50*time.Millisecond, "should not wait when duration=0")
}

// --- GracePeriodGate: cool-down then query -------------------------------

func TestGracePeriodGateWaitsThenPasses(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 50 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt", Label: "error-rate"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	)

	start := time.Now()
	r, err := g.Check(context.Background(), GateInput{RunID: "r1"})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(1), calls.Load(), "exactly one query after the cool-down")
	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond, "should wait at least the cool-down")
	assert.Less(t, elapsed, 200*time.Millisecond, "should not wait much longer than the cool-down")

	// The audit trail should record how long the wait actually was.
	waited := r.Details["waited_ms"].(int64)
	assert.GreaterOrEqual(t, waited, int64(50))
}

func TestGracePeriodGateWaitsThenFailsOnThreshold(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 30 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, "blocked_by_policy", r.Details["reason"])
}

func TestGracePeriodGateQueryError(t *testing.T) {
	srv, calls := promServer(promErrorResponse("bad query"), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(1),
		WithGracePeriodRetryDelay(2*time.Millisecond),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load(), "1 initial + 1 retry")
}

// --- GracePeriodGate: on-failure policies --------------------------------

func TestGracePeriodGateOnFailureWarn(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
		WithGracePeriodOnFailure(OnFailureWarn),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, "warn_by_policy", r.Details["reason"])
	assert.Contains(t, r.Message, "WARNING")
}

func TestGracePeriodGateOnFailureSkip(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
		WithGracePeriodOnFailure(OnFailureSkip),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed, "skip policy should make the gate pass")
	assert.Equal(t, "skipped_by_policy", r.Details["reason"])
}

// --- GracePeriodGate: MultiSLO -------------------------------------------

func TestGracePeriodGateMultiSLOAllPass(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("multi", GracePeriodConfig{
		Duration: 20 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt", Label: "error-rate"},
			{Query: "rate(p99[5m])", Threshold: 0.5, Compare: "lt", Label: "p99"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Equal(t, int64(2), calls.Load())
	assert.Contains(t, r.Message, "2/2 queries within threshold")
}

func TestGracePeriodGateMultiSLOOneFails(t *testing.T) {
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

	g := NewGracePeriodGate("multi", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "q1", Threshold: 0.01, Compare: "lt"},
			{Query: "q2", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)

	queries := r.Details["queries"].([]map[string]any)
	require.Len(t, queries, 2)
	assert.True(t, queries[0]["passed"].(bool))
	assert.False(t, queries[1]["passed"].(bool))
}

// --- GracePeriodGate: no queries configured ------------------------------

func TestGracePeriodGateNoQueriesConfigured(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("empty", GracePeriodConfig{
		Duration:   10 * time.Millisecond,
		SLOQueries: nil,
	},
		WithGracePeriodSource(srv.URL),
	)

	r, err := g.Check(context.Background(), GateInput{})
	require.Error(t, err, "no queries configured is a configuration error")
	assert.False(t, r.Passed)
	assert.Equal(t, "no_queries_configured", r.Details["reason"])
}

// --- GracePeriodGate: cancellation during cool-down ----------------------

func TestGracePeriodGateCancelledDuringCoolDown(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 200 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	r, err := g.Check(ctx, GateInput{})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, "cancelled_during_cooldown", r.Details["reason"])
	assert.Equal(t, int64(0), calls.Load(), "no query should be issued when cancelled during cool-down")
	// Should return shortly after the cancellation, not after the full 200ms.
	assert.Less(t, elapsed, 150*time.Millisecond)
}

func TestGracePeriodGateAlreadyCancelledContext(t *testing.T) {
	srv, calls := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Equal(t, int64(0), calls.Load())
	assert.Contains(t, r.Message, "cancelled before run")
}

func TestGracePeriodGateCancelledMidQuery(t *testing.T) {
	// Server that sleeps so the per-query timeout does not fire but the
	// caller's context does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.005)))
	}))
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodTimeout(30*time.Second),
		WithGracePeriodRetries(0),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
}

// --- GracePeriodGate: per-query timeout ----------------------------------

func TestGracePeriodGateQueryTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(promVectorResponse(0.005)))
	}))
	defer srv.Close()

	g := NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodTimeout(20*time.Millisecond),
		WithGracePeriodRetries(0),
	)

	start := time.Now()
	r, err := g.Check(context.Background(), GateInput{})
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.False(t, r.Passed)
	// 10ms cool-down + 20ms query timeout ≈ 30ms; well under the 200ms server delay.
	assert.Less(t, elapsed, 150*time.Millisecond)
}

// --- GracePeriodGate: integration with GateManager -----------------------

func TestGracePeriodGateRegisteredWithManager(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewGracePeriodGate("cool-down", GracePeriodConfig{
		Duration: 20 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "rate(errors[5m])", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	))

	results := gm.RunPhase(context.Background(), PhaseGracePeriod, GateInput{RunID: "r1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}

func TestGracePeriodGateManagerFailureTriggersSkip(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.02), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewGracePeriodGate("a-fail", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "q", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	))
	// slow-pass sorts after a-fail; it should be marked skipped.
	gm.Register(&mockGate{name: "b-slow", phase: PhaseGracePeriod, pass: true, delay: 200 * time.Millisecond})

	results := gm.RunPhase(context.Background(), PhaseGracePeriod, GateInput{})
	require.Len(t, results, 2)
	assert.False(t, results[0].Passed)
	assert.False(t, isSkipped(results[0]), "a-fail should be a real failure")
	assert.True(t, isSkipped(results[1]), "b-slow should be skipped")
}

// --- GracePeriodGate: phase isolation ------------------------------------

func TestGracePeriodGateIsolatedFromOtherPhases(t *testing.T) {
	srv, _ := promServer(promVectorResponse(0.005), http.StatusOK)
	defer srv.Close()

	gm := NewGateManager()
	gm.Register(NewGracePeriodGate("gp", GracePeriodConfig{
		Duration: 10 * time.Millisecond,
		SLOQueries: []SLOQuerySpec{
			{Query: "q", Threshold: 0.01, Compare: "lt"},
		},
	},
		WithGracePeriodSource(srv.URL),
		WithGracePeriodRetries(0),
	))
	gm.Register(NewNoopGate("pre", PhasePreApply, true))
	gm.Register(NewNoopGate("batch", PhasePostBatch, true))
	gm.Register(NewNoopGate("post", PhasePostApply, true))

	// The grace_period gate should only appear in PhaseGracePeriod.
	assert.Len(t, gm.Gates(PhaseGracePeriod), 1)
	assert.Len(t, gm.Gates(PhasePreApply), 1)
	assert.Len(t, gm.Gates(PhasePostBatch), 1)
	assert.Len(t, gm.Gates(PhasePostApply), 1)

	// Running the other phases should not invoke the grace_period gate.
	pre := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, pre, 1)
	assert.True(t, pre[0].Passed)
	assert.NotContains(t, pre[0].Message, "grace_period")
}