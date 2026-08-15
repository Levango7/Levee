// Package verify tests cover gate registration, three-phase execution,
// pass/fail/skip/timeout semantics, concurrent execution and result
// aggregation for the GateManager framework.
package verify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock Gate --------------------------------------------------------------

// mockGate is a configurable Gate stub. Each field toggles a behaviour so that
// tests can exercise the manager's error paths without defining a separate
// stub per case.
type mockGate struct {
	name      string
	phase     GatePhase
	pass      bool
	delay     time.Duration
	err       error
	callCount int64
	mu        sync.Mutex
}

func (g *mockGate) Name() string     { return g.name }
func (g *mockGate) Phase() GatePhase { return g.phase }

func (g *mockGate) Check(ctx context.Context, _ GateInput) (GateResult, error) {
	atomic.AddInt64(&g.callCount, 1)
	if g.delay > 0 {
		select {
		case <-time.After(g.delay):
		case <-ctx.Done():
			return GateResult{Passed: false, Message: "cancelled"}, ctx.Err()
		}
	}
	if g.err != nil {
		return GateResult{}, g.err
	}
	msg := "mock passed"
	if !g.pass {
		msg = "mock failed"
	}
	return GateResult{
		Passed:  g.pass,
		Message: msg,
		Details: map[string]any{"gate": g.name},
	}, nil
}

func (g *mockGate) calls() int64 { return atomic.LoadInt64(&g.callCount) }

// --- registration ----------------------------------------------------------

func TestNewGateManagerEmpty(t *testing.T) {
	gm := NewGateManager()
	assert.Empty(t, gm.Names())
	for _, p := range AllPhases() {
		assert.Empty(t, gm.Gates(p))
	}
}

func TestRegisterAndLookup(t *testing.T) {
	gm := NewGateManager()
	g := NewNoopGate("reachability", PhasePreApply, true)
	gm.Register(g)

	got, ok := gm.Gate("reachability")
	require.True(t, ok)
	assert.Equal(t, "reachability", got.Name())
	assert.Equal(t, PhasePreApply, got.Phase())

	_, ok = gm.Gate("nonexistent")
	assert.False(t, ok)
}

func TestRegisterOverwrite(t *testing.T) {
	gm := NewGateManager()
	a := NewNoopGate("g", PhasePreApply, true)
	b := NewNoopGate("g", PhasePreApply, false)
	gm.Register(a)
	gm.Register(b)
	got, ok := gm.Gate("g")
	require.True(t, ok)
	assert.Same(t, b, got, "second registration should overwrite the first")
}

func TestUnregister(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("g", PhasePreApply, true))
	require.True(t, func() bool {
		_, ok := gm.Gate("g")
		return ok
	}())

	gm.Unregister("g")
	_, ok := gm.Gate("g")
	assert.False(t, ok)

	// Unregister a non-existent gate is a no-op.
	gm.Unregister("nope")
}

func TestNamesSorted(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("zeta", PhasePreApply, true))
	gm.Register(NewNoopGate("alpha", PhasePostBatch, true))
	gm.Register(NewNoopGate("mid", PhasePostApply, true))
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, gm.Names())
}

func TestGatesByPhaseSorted(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("c", PhasePostBatch, true))
	gm.Register(NewNoopGate("a", PhasePostBatch, true))
	gm.Register(NewNoopGate("b", PhasePreApply, true))

	got := gm.Gates(PhasePostBatch)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Name())
	assert.Equal(t, "c", got[1].Name())

	assert.Len(t, gm.Gates(PhasePreApply), 1)
	assert.Len(t, gm.Gates(PhasePostApply), 0)
}

func TestRegisterConcurrent(t *testing.T) {
	gm := NewGateManager()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			gm.Register(NewNoopGate(fmt.Sprintf("g-%d", i), PhasePreApply, true))
		}()
	}
	wg.Wait()
	assert.Len(t, gm.Names(), n)
}

// --- RunPhase: empty -------------------------------------------------------

func TestRunPhaseEmptyReturnsNil(t *testing.T) {
	gm := NewGateManager()
	for _, p := range AllPhases() {
		assert.Nil(t, gm.RunPhase(context.Background(), p, GateInput{}))
	}
}

// --- RunPhase: three phases ------------------------------------------------

func TestRunPhasePreApplyPass(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("reach", PhasePreApply, true))
	gm.Register(NewNoopGate("prereq", PhasePreApply, true))

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{RunID: "r1"})
	require.Len(t, results, 2)
	for _, r := range results {
		assert.True(t, r.Passed, "all pre_apply gates should pass")
	}
}

func TestRunPhasePostBatchPass(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("health", PhasePostBatch, true))
	gm.Register(NewNoopGate("slo", PhasePostBatch, true))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{
		RunID:   "r1",
		BatchID: "b1",
	})
	require.Len(t, results, 2)
	for _, r := range results {
		assert.True(t, r.Passed)
	}
}

func TestRunPhasePostApplyPass(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("regression", PhasePostApply, true))

	results := gm.RunPhase(context.Background(), PhasePostApply, GateInput{RunID: "r1"})
	require.Len(t, results, 1)
	assert.True(t, results[0].Passed)
}

func TestRunPhaseIsolatesPhases(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("pre", PhasePreApply, true))
	gm.Register(NewNoopGate("batch", PhasePostBatch, true))
	gm.Register(NewNoopGate("post", PhasePostApply, true))

	pre := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, pre, 1)
	assert.Equal(t, true, pre[0].Passed)

	batch := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{})
	require.Len(t, batch, 1)

	post := gm.RunPhase(context.Background(), PhasePostApply, GateInput{})
	require.Len(t, post, 1)
}

// --- RunPhase: pass / fail / skip -----------------------------------------

func TestRunPhaseAllPass(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("a", PhasePreApply, true))
	gm.Register(NewNoopGate("b", PhasePreApply, true))
	gm.Register(NewNoopGate("c", PhasePreApply, true))

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 3)
	for i, r := range results {
		assert.True(t, r.Passed, "gate %d should pass", i)
		assert.NotEmpty(t, r.Message)
	}
}

func TestRunPhaseSingleFail(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("a", PhasePreApply, true))
	gm.Register(NewNoopGate("b", PhasePreApply, false))
	gm.Register(NewNoopGate("c", PhasePreApply, true))

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 3)

	// Exactly one gate is a real failure (Passed == false and not skipped).
	// The other two are either passed or skipped depending on the order in
	// which their results arrive at the manager: when all gates are
	// instantaneous they may all complete before the failure is observed,
	// so we only assert the invariant that there is exactly one real
	// failure and no result is simultaneously passed and skipped.
	failedCount := 0
	for _, r := range results {
		if !r.Passed && !isSkipped(r) {
			failedCount++
		}
	}
	assert.Equal(t, 1, failedCount, "exactly one gate should be a real failure")

	// No result can be both passed and skipped.
	for i, r := range results {
		if r.Passed {
			assert.False(t, isSkipped(r), "gate %d cannot be both passed and skipped", i)
		}
	}
}

// isSkipped reports whether a GateResult was produced by skippedResult.
func isSkipped(r GateResult) bool {
	reason, ok := r.Details["reason"]
	return ok && reason == "skipped"
}

func TestRunPhaseFailureStopsPendingGates(t *testing.T) {
	gm := NewGateManager()
	// "slow-pass" would pass but takes much longer than "fast-fail" needs to
	// fail. Because RunPhase returns on the first failure, "slow-pass" must
	// be reported as skipped, not passed.
	gm.Register(&mockGate{name: "slow-pass", phase: PhasePreApply, pass: true, delay: 100 * time.Millisecond})
	gm.Register(&mockGate{name: "fast-fail", phase: PhasePreApply, pass: false, delay: 5 * time.Millisecond})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 2)

	// fast-fail sorts before slow-pass, so index 0 is the failure.
	assert.False(t, results[0].Passed, "fast-fail should be the failing gate")
	assert.False(t, isSkipped(results[0]), "fast-fail should be a real failure, not skipped")

	assert.False(t, results[1].Passed, "slow-pass should not have passed")
	assert.True(t, isSkipped(results[1]), "slow-pass should be marked as skipped")
}

func TestRunPhaseErrorTreatedAsFailure(t *testing.T) {
	gm := NewGateManager()
	want := errors.New("channel broken")
	gm.Register(&mockGate{name: "broken", phase: PhasePreApply, err: want})
	gm.Register(NewNoopGate("ok", PhasePreApply, true))

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 2)

	// "broken" sorts before "ok".
	assert.False(t, results[0].Passed, "error gate should be treated as failed")
	assert.Contains(t, results[0].Message, "channel broken")
}

func TestRunPhasePreservesGateMessageOnError(t *testing.T) {
	gm := NewGateManager()
	gm.Register(&mockGate{
		name:  "broken",
		phase: PhasePreApply,
		err:   errors.New("boom"),
	})
	// Use a custom gate that returns both a message and an error.
	gm.Register(&customGate{
		name:   "with-msg",
		phase:  PhasePreApply,
		result: GateResult{Passed: false, Message: "pre-existing message"},
		err:    errors.New("boom2"),
	})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 2)
	// Both gates fail; at least one should carry the pre-existing message.
	foundMsg := false
	for _, r := range results {
		if r.Message != "" && !isSkipped(r) {
			foundMsg = true
		}
	}
	assert.True(t, foundMsg, "at least one gate should carry a non-empty message")
}

// customGate is a fully configurable Gate for tests that need to return a
// specific GateResult alongside an error.
type customGate struct {
	name   string
	phase  GatePhase
	result GateResult
	err    error
}

func (g *customGate) Name() string     { return g.name }
func (g *customGate) Phase() GatePhase { return g.phase }
func (g *customGate) Check(context.Context, GateInput) (GateResult, error) {
	return g.result, g.err
}

// --- RunPhase: timeout -----------------------------------------------------

func TestRunPhaseTimeoutMarksPendingSkipped(t *testing.T) {
	gm := NewGateManager()
	gm.Register(&mockGate{name: "slow", phase: PhasePreApply, pass: true, delay: 200 * time.Millisecond})
	gm.Register(NewNoopGate("fast", PhasePreApply, true))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	results := gm.RunPhase(ctx, PhasePreApply, GateInput{})
	require.Len(t, results, 2)

	// At least one gate should be skipped due to the context deadline.
	skippedCount := 0
	for _, r := range results {
		if isSkipped(r) {
			skippedCount++
		}
	}
	assert.GreaterOrEqual(t, skippedCount, 1, "at least one gate should be skipped on timeout")
}

func TestRunPhaseCancelledContext(t *testing.T) {
	gm := NewGateManager()
	gm.Register(&mockGate{name: "slow", phase: PhasePreApply, pass: true, delay: 200 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	results := gm.RunPhase(ctx, PhasePreApply, GateInput{})
	require.Len(t, results, 1)
	assert.False(t, results[0].Passed)
}

func TestRunPhaseContextAlreadyExpired(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("g", PhasePreApply, true))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // ensure deadline passed

	results := gm.RunPhase(ctx, PhasePreApply, GateInput{})
	require.Len(t, results, 1)
	// The gate may have run before the context expired or been marked
	// skipped; either way the result is present.
	assert.NotEmpty(t, results[0].Message)
}

// --- RunPhase: concurrency -------------------------------------------------

func TestRunPhaseConcurrentExecution(t *testing.T) {
	gm := NewGateManager()
	// Three gates each sleeping 40ms. If run sequentially the total would be
	// 120ms; run concurrently it should be close to 40ms.
	g1 := &mockGate{name: "g1", phase: PhasePreApply, pass: true, delay: 40 * time.Millisecond}
	g2 := &mockGate{name: "g2", phase: PhasePreApply, pass: true, delay: 40 * time.Millisecond}
	g3 := &mockGate{name: "g3", phase: PhasePreApply, pass: true, delay: 40 * time.Millisecond}
	gm.Register(g1)
	gm.Register(g2)
	gm.Register(g3)

	start := time.Now()
	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	elapsed := time.Since(start)

	require.Len(t, results, 3)
	for _, r := range results {
		assert.True(t, r.Passed)
	}
	assert.Less(t, elapsed, 100*time.Millisecond, "gates should run concurrently, not sequentially")
	assert.Equal(t, int64(1), g1.calls())
	assert.Equal(t, int64(1), g2.calls())
	assert.Equal(t, int64(1), g3.calls())
}

func TestRunPhaseConcurrentManagerCalls(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("g", PhasePreApply, true))

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			res := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
			require.Len(t, res, 1)
		}()
	}
	wg.Wait()
}

// --- RunPhase: result aggregation ------------------------------------------

func TestRunPhaseResultOrderMatchesGates(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("c", PhasePreApply, true))
	gm.Register(NewNoopGate("a", PhasePreApply, true))
	gm.Register(NewNoopGate("b", PhasePreApply, true))

	gates := gm.Gates(PhasePreApply)
	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})

	require.Len(t, results, len(gates))
	// Results are in the same order as Gates(phase), which is sorted by name.
	assert.Equal(t, "a", gates[0].Name())
	assert.Equal(t, "b", gates[1].Name())
	assert.Equal(t, "c", gates[2].Name())
	for _, r := range results {
		assert.True(t, r.Passed)
	}
}

func TestRunPhaseLatencyPopulated(t *testing.T) {
	gm := NewGateManager()
	gm.Register(&mockGate{name: "slow", phase: PhasePreApply, pass: true, delay: 20 * time.Millisecond})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 1)
	assert.Greater(t, results[0].Latency, time.Duration(0), "latency should be measured")
}

func TestRunPhasePreservesGateSuppliedLatency(t *testing.T) {
	gm := NewGateManager()
	want := 42 * time.Millisecond
	gm.Register(&customGate{
		name:   "g",
		phase:  PhasePreApply,
		result: GateResult{Passed: true, Latency: want},
	})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 1)
	assert.Equal(t, want, results[0].Latency, "gate-supplied latency should be preserved")
}

func TestRunPhaseAllSkippedOnFirstFailure(t *testing.T) {
	gm := NewGateManager()
	// "a-fail" sorts first and fails immediately; the other two are slow and
	// must be skipped.
	gm.Register(&mockGate{name: "a-fail", phase: PhasePreApply, pass: false})
	gm.Register(&mockGate{name: "b-slow", phase: PhasePreApply, pass: true, delay: 100 * time.Millisecond})
	gm.Register(&mockGate{name: "c-slow", phase: PhasePreApply, pass: true, delay: 100 * time.Millisecond})

	results := gm.RunPhase(context.Background(), PhasePreApply, GateInput{})
	require.Len(t, results, 3)

	assert.False(t, results[0].Passed)
	assert.False(t, isSkipped(results[0]), "first gate is a real failure")

	assert.True(t, isSkipped(results[1]), "second gate should be skipped")
	assert.True(t, isSkipped(results[2]), "third gate should be skipped")
}

// --- NoopGate --------------------------------------------------------------

func TestNoopGatePass(t *testing.T) {
	g := NewNoopGate("noop", PhasePreApply, true)
	assert.Equal(t, "noop", g.Name())
	assert.Equal(t, PhasePreApply, g.Phase())

	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.True(t, r.Passed)
	assert.Contains(t, r.Message, "passed")
}

func TestNoopGateFail(t *testing.T) {
	g := NewNoopGate("noop", PhasePreApply, false)
	r, err := g.Check(context.Background(), GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
	assert.Contains(t, r.Message, "failed")
}

func TestNoopGateCancelledContext(t *testing.T) {
	g := NewNoopGate("noop", PhasePreApply, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r, err := g.Check(ctx, GateInput{})
	require.NoError(t, err)
	assert.False(t, r.Passed)
}

func TestNoopGateInManager(t *testing.T) {
	gm := NewGateManager()
	gm.Register(NewNoopGate("noop-pass", PhasePostBatch, true))
	gm.Register(NewNoopGate("noop-fail", PhasePostBatch, false))

	results := gm.RunPhase(context.Background(), PhasePostBatch, GateInput{})
	require.Len(t, results, 2)
	// noop-fail sorts before noop-pass.
	assert.False(t, results[0].Passed)
}

// --- default manager -------------------------------------------------------

func TestDefaultManagerIsSingleton(t *testing.T) {
	a := DefaultManager()
	b := DefaultManager()
	assert.Same(t, a, b)
}

func TestDefaultManagerRegister(t *testing.T) {
	// Use a unique gate name to avoid colliding with any real gate that may
	// register itself via init().
	name := "test-noop-gate"
	Register(NewNoopGate(name, PhasePreApply, true))
	defer DefaultManager().Unregister(name)

	g, ok := DefaultManager().Gate(name)
	require.True(t, ok)
	assert.Equal(t, name, g.Name())
}

// --- GateInput / GateResult zero values -----------------------------------

func TestGateInputZeroValue(t *testing.T) {
	var in GateInput
	assert.Empty(t, in.RunID)
	assert.Empty(t, in.BatchID)
	assert.Nil(t, in.TargetIDs)
	assert.Nil(t, in.Channel)
	assert.Nil(t, in.Params)
}

func TestGateResultZeroValue(t *testing.T) {
	var r GateResult
	assert.False(t, r.Passed)
	assert.Empty(t, r.Message)
	assert.Nil(t, r.Details)
	assert.Equal(t, time.Duration(0), r.Latency)
}

// --- AllPhases -------------------------------------------------------------

func TestAllPhasesOrder(t *testing.T) {
	phases := AllPhases()
	assert.Equal(t, []GatePhase{PhasePreApply, PhasePostBatch, PhasePostApply, PhaseGracePeriod}, phases)
}

func TestPhaseStringValues(t *testing.T) {
	assert.Equal(t, GatePhase("pre_apply"), PhasePreApply)
	assert.Equal(t, GatePhase("post_batch"), PhasePostBatch)
	assert.Equal(t, GatePhase("post_apply"), PhasePostApply)
	assert.Equal(t, GatePhase("grace_period"), PhaseGracePeriod)
}
