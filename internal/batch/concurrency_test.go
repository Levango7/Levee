package batch

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/plan"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- basic acquire / release -----------------------------------------------

func TestConcurrencyManagerAcquireRelease(t *testing.T) {
	limiter := channel.NewLimiter(1, 0, 0, 0) // global cap 1
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)
	release, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	require.NotNil(t, release)

	stats := mgr.Stats()
	assert.Equal(t, int64(1), stats.InFlight, "one permit in flight after Acquire")
	assert.Equal(t, int64(1), stats.TotalAcquired)

	release()
	stats = mgr.Stats()
	assert.Equal(t, int64(0), stats.InFlight, "no permits in flight after Release")
	assert.Equal(t, int64(1), stats.TotalAcquired, "TotalAcquired is cumulative")
}

func TestConcurrencyManagerNilLimiter(t *testing.T) {
	mgr := NewConcurrencyManager(nil, "batch", 0)

	// A nil limiter means unlimited: Acquire always succeeds immediately.
	release, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	require.NotNil(t, release)

	stats := mgr.Stats()
	assert.Equal(t, int64(1), stats.InFlight)
	assert.Equal(t, int64(1), stats.TotalAcquired)

	release()
	stats = mgr.Stats()
	assert.Equal(t, int64(0), stats.InFlight)
}

func TestConcurrencyManagerReleaseIdempotent(t *testing.T) {
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)
	release, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)

	release()
	release() // must not panic and must not double-decrement
	release()

	stats := mgr.Stats()
	assert.Equal(t, int64(0), stats.InFlight, "idempotent Release must not underflow InFlight")
}

// --- concurrency cap & queueing --------------------------------------------

func TestConcurrencyManagerConcurrencyCap(t *testing.T) {
	// Global cap 2: at most 2 permits held at once.
	limiter := channel.NewLimiter(2, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	var current int32
	var maxObserved int32
	var wg sync.WaitGroup

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := "h" + string(rune('0'+idx))
			release, err := mgr.Acquire(context.Background(), target)
			if err != nil {
				return
			}
			defer release()

			cur := atomic.AddInt32(&current, 1)
			for {
				m := atomic.LoadInt32(&maxObserved)
				if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt32(&current, -1)
		}(i)
	}
	wg.Wait()

	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(2),
		"limiter cap 2 must cap in-flight permits, observed=%d",
		atomic.LoadInt32(&maxObserved))
	assert.Equal(t, int32(2), atomic.LoadInt32(&maxObserved),
		"with 6 acquirers and cap 2 the high-water mark should reach 2")

	stats := mgr.Stats()
	assert.Equal(t, int64(6), stats.TotalAcquired, "all 6 acquirers should eventually succeed")
	assert.Equal(t, int64(0), stats.InFlight, "all permits released")
}

func TestConcurrencyManagerQueueing(t *testing.T) {
	// Global cap 1: acquirers must queue, not get rejected.
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	var acquired int32
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := "h" + string(rune('0'+idx))
			release, err := mgr.Acquire(context.Background(), target)
			if err != nil {
				return
			}
			defer release()
			atomic.AddInt32(&acquired, 1)
			time.Sleep(30 * time.Millisecond)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// All 4 acquirers should have succeeded (queueing, not rejection).
	assert.Equal(t, int32(4), atomic.LoadInt32(&acquired),
		"queueing should let all acquirers through eventually")
	// 4 * 30ms = 120ms minimum when serialised through a cap-1 limiter.
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond,
		"cap-1 should serialise acquirers, elapsed=%v", elapsed)
}

// --- context cancellation --------------------------------------------------

func TestConcurrencyManagerContextCancelledBeforeStart(t *testing.T) {
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Acquire

	release, err := mgr.Acquire(ctx, "h1")
	require.Error(t, err)
	assert.Nil(t, release)
	assert.Contains(t, err.Error(), "cancelled before start")

	stats := mgr.Stats()
	assert.Equal(t, int64(1), stats.TotalTimedOut, "pre-cancelled Acquire counts as a failure")
}

func TestConcurrencyManagerContextCancelledWhileWaiting(t *testing.T) {
	// Global cap 1: hold the only permit, then have a second Acquire
	// wait and cancel its context. The waiting Acquire must return
	// promptly with an error, and the held permit must remain valid.
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	// Hold the only permit.
	holder, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	defer holder()

	// Second Acquire waits, then we cancel its context.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := mgr.Acquire(ctx, "h2")
		done <- err
	}()

	// Give the waiter a moment to register, then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire hung after context cancellation")
	}

	// The holder's permit is still valid: releasing it must succeed
	// without panicking, and a new Acquire must succeed immediately.
	holder()
	release2, err := mgr.Acquire(context.Background(), "h3")
	require.NoError(t, err)
	release2()
}

// --- wait timeout ----------------------------------------------------------

func TestConcurrencyManagerTimeout(t *testing.T) {
	// Global cap 1: hold the only permit, then have a second Acquire
	// time out after 50ms.
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 50*time.Millisecond)

	holder, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	defer holder()

	start := time.Now()
	release, err := mgr.Acquire(context.Background(), "h2")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, release)
	assert.True(t, errors.Is(err, ErrAcquireTimeout), "err should be ErrAcquireTimeout, got: %v", err)
	// Should time out close to 50ms, well under 1s.
	assert.Less(t, elapsed, 500*time.Millisecond,
		"timeout should fire near 50ms, elapsed=%v", elapsed)

	stats := mgr.Stats()
	assert.Equal(t, int64(1), stats.TotalTimedOut)
}

func TestConcurrencyManagerTimeoutZeroMeansNoTimeout(t *testing.T) {
	// waitTimeout=0 means Acquire relies on ctx to bound the wait.
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	holder, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	defer holder()

	// With no wait timeout, only ctx can bound the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	release, err := mgr.Acquire(ctx, "h2")
	require.Error(t, err)
	assert.Nil(t, release)
	assert.Contains(t, err.Error(), "cancelled")
}

// --- closed manager --------------------------------------------------------

func TestConcurrencyManagerClosedReturnsError(t *testing.T) {
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)
	mgr.Close()

	release, err := mgr.Acquire(context.Background(), "h1")
	require.Error(t, err)
	assert.Nil(t, release)
	assert.True(t, errors.Is(err, ErrManagerClosed))

	stats := mgr.Stats()
	assert.Equal(t, int64(1), stats.TotalTimedOut)
}

func TestConcurrencyManagerCloseIsIdempotent(t *testing.T) {
	mgr := NewConcurrencyManager(nil, "batch", 0)
	mgr.Close()
	mgr.Close() // must not panic
	mgr.Close()
}

// --- statistics ------------------------------------------------------------

func TestConcurrencyManagerStats(t *testing.T) {
	limiter := channel.NewLimiter(2, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	// Initial stats.
	stats := mgr.Stats()
	assert.Equal(t, ConcurrencyStats{}, stats)

	// Acquire two permits.
	r1, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	r2, err := mgr.Acquire(context.Background(), "h2")
	require.NoError(t, err)

	stats = mgr.Stats()
	assert.Equal(t, int64(2), stats.InFlight)
	assert.Equal(t, int64(2), stats.TotalAcquired)
	assert.Equal(t, int64(0), stats.TotalTimedOut)

	// Release one.
	r1()
	stats = mgr.Stats()
	assert.Equal(t, int64(1), stats.InFlight)
	assert.Equal(t, int64(2), stats.TotalAcquired)

	// Release the other.
	r2()
	stats = mgr.Stats()
	assert.Equal(t, int64(0), stats.InFlight)
	assert.Equal(t, int64(2), stats.TotalAcquired)

	// A failed Acquire (timeout) increments TotalTimedOut. We use a
	// separate cap-1 limiter that we saturate so the second Acquire
	// times out.
	saturLimiter := channel.NewLimiter(1, 0, 0, 0)
	defer saturLimiter.Close()
	mgr2 := NewConcurrencyManager(saturLimiter, "batch", 30*time.Millisecond)
	holder, err := mgr2.Acquire(context.Background(), "h0")
	require.NoError(t, err)
	defer holder()
	_, err = mgr2.Acquire(context.Background(), "h4")
	require.Error(t, err)
	stats2 := mgr2.Stats()
	assert.Equal(t, int64(1), stats2.TotalTimedOut)
}

func TestConcurrencyManagerWaitingCount(t *testing.T) {
	// Global cap 1: hold the permit, then start two waiters and
	// observe Waiting == 2 (after they have registered).
	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()

	mgr := NewConcurrencyManager(limiter, "batch", 0)

	holder, err := mgr.Acquire(context.Background(), "h1")
	require.NoError(t, err)
	defer holder()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			target := "h" + string(rune('0'+idx))
			// Use a short timeout so the test doesn't hang if the
			// waiting count never reaches 2.
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = mgr.Acquire(ctx, target)
		}(i)
	}

	// Wait until both waiters have registered (or timeout).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if mgr.Stats().Waiting >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stats := mgr.Stats()
	assert.GreaterOrEqual(t, stats.Waiting, int64(1),
		"at least one waiter should be registered, stats=%+v", stats)

	wg.Wait()
}

// --- integration with Controller -------------------------------------------

func TestControllerWithConcurrencyManager(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5"}
	// Batch allows 5 concurrent, but the manager caps global to 2.
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 5})

	limiter := channel.NewLimiter(2, 0, 0, 0)
	defer limiter.Close()
	mgr := NewConcurrencyManager(limiter, "batch", 0)
	defer mgr.Close()

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController(WithConcurrencyManager(mgr))
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)
	assert.LessOrEqual(t, atomic.LoadInt32(&maxObserved), int32(2),
		"manager should cap concurrency to 2, observed=%d",
		atomic.LoadInt32(&maxObserved))
	assert.Equal(t, int32(2), atomic.LoadInt32(&maxObserved))

	// The manager should have acquired 5 permits total.
	stats := mgr.Stats()
	assert.Equal(t, int64(5), stats.TotalAcquired)
	assert.Equal(t, int64(0), stats.InFlight, "all permits released after Execute")
}

func TestControllerManagerTakesPrecedenceOverLimiter(t *testing.T) {
	// When both a manager and a raw limiter are configured, the manager
	// takes precedence. We verify by giving the manager a cap of 1 and
	// the raw limiter a cap of 5: the observed cap should be 1.
	targets := []string{"h1", "h2", "h3", "h4"}
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: targets, MaxConcurrency: 4})

	rawLimiter := channel.NewLimiter(5, 0, 0, 0)
	defer rawLimiter.Close()
	mgrLimiter := channel.NewLimiter(1, 0, 0, 0)
	defer mgrLimiter.Close()
	mgr := NewConcurrencyManager(mgrLimiter, "batch", 0)
	defer mgr.Close()

	var current int32
	var maxObserved int32
	execFn := func(_ context.Context, _ plan.Batch, _ string, _ plan.PlanStep) error {
		cur := atomic.AddInt32(&current, 1)
		for {
			m := atomic.LoadInt32(&maxObserved)
			if cur <= m || atomic.CompareAndSwapInt32(&maxObserved, m, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&current, -1)
		return nil
	}

	c := NewController(
		WithConcurrencyLimiter(rawLimiter),
		WithConcurrencyManager(mgr),
	)
	results := c.Execute(context.Background(), p, execFn)
	require.Len(t, results, 1)
	assert.NoError(t, results[0].Error)
	assert.Equal(t, int32(1), atomic.LoadInt32(&maxObserved),
		"manager (cap 1) should take precedence over raw limiter (cap 5)")
}

func TestControllerManagerClosedReturnsError(t *testing.T) {
	p := makePlan(struct {
		Targets        []string
		MaxConcurrency int
	}{Targets: []string{"h1"}, MaxConcurrency: 1})

	limiter := channel.NewLimiter(1, 0, 0, 0)
	defer limiter.Close()
	mgr := NewConcurrencyManager(limiter, "batch", 0)
	mgr.Close() // closed before use

	c := NewController(WithConcurrencyManager(mgr))
	results := c.Execute(context.Background(), p, noopExec)
	require.Len(t, results, 1)
	require.Error(t, results[0].Error)
	assert.Contains(t, results[0].Error.Error(), "concurrency")
}
