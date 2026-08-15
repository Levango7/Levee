package channel

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Limiter construction ---------------------------------------------------

func TestNewLimiterDefaults(t *testing.T) {
	l := NewLimiter(50, 20, 5, 10*time.Second)
	defer l.Close()

	stats := l.Stats()
	assert.Equal(t, 50, stats.GlobalMax)
	assert.Equal(t, 20, stats.ChannelMax)
	assert.Equal(t, 5, stats.TargetMax)
	assert.Equal(t, 0, stats.GlobalInUse)
	assert.Empty(t, stats.ChannelInUse)
	assert.Empty(t, stats.TargetInUse)
}

func TestNewLimiterUnlimitedTier(t *testing.T) {
	// A zero/negative cap means unlimited for that tier.
	l := NewLimiter(0, 0, 0, 0)
	defer l.Close()

	// Should never block.
	for i := 0; i < 1000; i++ {
		require.NoError(t, l.Acquire("ssh", "host1"))
	}
	// Release all.
	for i := 0; i < 1000; i++ {
		l.Release("ssh", "host1")
	}
}

// --- L1: global concurrency -------------------------------------------------

func TestLimiterGlobalCap(t *testing.T) {
	l := NewLimiter(3, 10, 10, 0)
	defer l.Close()

	// Acquire 3 permits across different channels/targets.
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h2"))
	require.NoError(t, l.Acquire("winrm", "h3"))

	stats := l.Stats()
	assert.Equal(t, 3, stats.GlobalInUse)

	// 4th should block; use a goroutine + timeout to verify.
	done := make(chan error, 1)
	go func() {
		done <- l.Acquire("ssh", "h4")
	}()

	select {
	case err := <-done:
		t.Fatalf("4th Acquire returned before a Release: %v", err)
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	// Release one; the 4th should now succeed.
	l.Release("ssh", "h1")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("4th Acquire did not succeed after Release")
	}

	// Clean up.
	l.Release("ssh", "h2")
	l.Release("winrm", "h3")
	l.Release("ssh", "h4")
}

// --- L2: per-channel concurrency --------------------------------------------

func TestLimiterPerChannelCap(t *testing.T) {
	l := NewLimiter(100, 2, 100, 0)
	defer l.Close()

	// Two acquires for "ssh" should succeed.
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h2"))

	stats := l.Stats()
	assert.Equal(t, 2, stats.ChannelInUse["ssh"])

	// Third "ssh" acquire should block.
	done := make(chan error, 1)
	go func() {
		done <- l.Acquire("ssh", "h3")
	}()

	select {
	case err := <-done:
		t.Fatalf("3rd ssh Acquire returned before Release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A "winrm" acquire should succeed immediately (different channel).
	require.NoError(t, l.Acquire("winrm", "h4"))

	// Release one ssh; the 3rd should now succeed.
	l.Release("ssh", "h1")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("3rd ssh Acquire did not succeed after Release")
	}

	// Clean up.
	l.Release("ssh", "h2")
	l.Release("ssh", "h3")
	l.Release("winrm", "h4")
}

// --- L3: per-target concurrency ---------------------------------------------

func TestLimiterPerTargetCap(t *testing.T) {
	l := NewLimiter(100, 100, 2, 0)
	defer l.Close()

	// Two acquires for the same target should succeed.
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h1"))

	stats := l.Stats()
	assert.Equal(t, 2, stats.TargetInUse["ssh:h1"])

	// Third acquire for the same target should block.
	done := make(chan error, 1)
	go func() {
		done <- l.Acquire("ssh", "h1")
	}()

	select {
	case err := <-done:
		t.Fatalf("3rd per-target Acquire returned before Release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A different target should succeed immediately.
	require.NoError(t, l.Acquire("ssh", "h2"))

	// Release one; the 3rd should succeed.
	l.Release("ssh", "h1")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("3rd per-target Acquire did not succeed after Release")
	}

	// Clean up.
	l.Release("ssh", "h1")
	l.Release("ssh", "h1")
	l.Release("ssh", "h2")
}

// --- Back-pressure: queuing -------------------------------------------------

func TestLimiterBackPressureQueueing(t *testing.T) {
	// global=1, generous per-channel/per-target, no timeout.
	l := NewLimiter(1, 100, 100, 0)
	defer l.Close()

	require.NoError(t, l.Acquire("ssh", "h1"))

	// Launch 5 waiters; they should queue, not fail.
	const waiters = 5
	var wg sync.WaitGroup
	wg.Add(waiters)
	var acquired int32
	for i := 0; i < waiters; i++ {
		go func(i int) {
			defer wg.Done()
			if err := l.Acquire("ssh", "h1"); err == nil {
				atomic.AddInt32(&acquired, 1)
				// Hold briefly then release.
				time.Sleep(10 * time.Millisecond)
				l.Release("ssh", "h1")
			}
		}(i)
	}

	// Give the waiters a moment to queue.
	time.Sleep(20 * time.Millisecond)

	// Release the initial permit; the queue should drain.
	l.Release("ssh", "h1")
	wg.Wait()

	assert.Equal(t, int32(waiters), atomic.LoadInt32(&acquired),
		"all waiters should eventually acquire after the initial Release")
}

// --- Timeout: ErrRateLimited ------------------------------------------------

func TestLimiterTimeoutReturnsErrRateLimited(t *testing.T) {
	l := NewLimiter(1, 100, 100, 50*time.Millisecond)
	defer l.Close()

	require.NoError(t, l.Acquire("ssh", "h1"))

	// Second acquire should time out and return ErrRateLimited.
	err := l.Acquire("ssh", "h1")
	require.ErrorIs(t, err, ErrRateLimited)

	stats := l.Stats()
	assert.Equal(t, int64(1), stats.TotalAcquired)
	assert.Equal(t, int64(1), stats.TotalTimedOut)

	l.Release("ssh", "h1")
}

func TestLimiterTimeoutAtL2(t *testing.T) {
	// global is generous; per-channel is the bottleneck.
	l := NewLimiter(100, 1, 100, 50*time.Millisecond)
	defer l.Close()

	require.NoError(t, l.Acquire("ssh", "h1"))
	err := l.Acquire("ssh", "h2")
	require.ErrorIs(t, err, ErrRateLimited)

	l.Release("ssh", "h1")
}

func TestLimiterTimeoutAtL3(t *testing.T) {
	// global and per-channel are generous; per-target is the bottleneck.
	l := NewLimiter(100, 100, 1, 50*time.Millisecond)
	defer l.Close()

	require.NoError(t, l.Acquire("ssh", "h1"))
	err := l.Acquire("ssh", "h1")
	require.ErrorIs(t, err, ErrRateLimited)

	l.Release("ssh", "h1")
}

// --- Stats accuracy ---------------------------------------------------------

func TestLimiterStatsAccuracy(t *testing.T) {
	l := NewLimiter(10, 5, 3, 0)
	defer l.Close()

	// Acquire a mix of permits.
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h2"))
	require.NoError(t, l.Acquire("winrm", "h3"))

	stats := l.Stats()
	assert.Equal(t, 4, stats.GlobalInUse)
	assert.Equal(t, 3, stats.ChannelInUse["ssh"])
	assert.Equal(t, 1, stats.ChannelInUse["winrm"])
	assert.Equal(t, 2, stats.TargetInUse["ssh:h1"])
	assert.Equal(t, 1, stats.TargetInUse["ssh:h2"])
	assert.Equal(t, 1, stats.TargetInUse["winrm:h3"])
	assert.Equal(t, int64(4), stats.TotalAcquired)
	assert.Equal(t, int64(0), stats.TotalTimedOut)

	// Release one and re-check.
	l.Release("ssh", "h1")
	stats = l.Stats()
	assert.Equal(t, 3, stats.GlobalInUse)
	assert.Equal(t, 2, stats.ChannelInUse["ssh"])
	assert.Equal(t, 1, stats.TargetInUse["ssh:h1"])

	// Clean up.
	l.Release("ssh", "h1")
	l.Release("ssh", "h2")
	l.Release("winrm", "h3")
}

// --- Concurrency stress test ------------------------------------------------

func TestLimiterConcurrentStress(t *testing.T) {
	// Tight caps to force contention.
	l := NewLimiter(4, 2, 1, 0)
	defer l.Close()

	const goroutines = 20
	const iterations = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var failures int32

	for i := 0; i < goroutines; i++ {
		go func(id int) {
			defer wg.Done()
			chType := "ssh"
			if id%2 == 0 {
				chType = "winrm"
			}
			target := "h" + string(rune('a'+(id%4)))
			for j := 0; j < iterations; j++ {
				if err := l.Acquire(chType, target); err != nil {
					atomic.AddInt32(&failures, 1)
					continue
				}
				// Hold briefly.
				time.Sleep(100 * time.Microsecond)
				l.Release(chType, target)
			}
		}(i)
	}

	wg.Wait()
	assert.Equal(t, int32(0), atomic.LoadInt32(&failures), "no Acquire should fail with infinite timeout")

	stats := l.Stats()
	assert.Equal(t, 0, stats.GlobalInUse, "all permits should be returned after stress test")
}

// --- Close behavior ---------------------------------------------------------

func TestLimiterCloseRejectsAcquire(t *testing.T) {
	l := NewLimiter(10, 10, 10, 0)
	l.Close()

	err := l.Acquire("ssh", "h1")
	require.ErrorIs(t, err, ErrLimiterClosed)
}

func TestLimiterCloseIdempotent(t *testing.T) {
	l := NewLimiter(10, 10, 10, 0)
	l.Close()
	l.Close() // must not panic
}

// --- Acquire/Release pairing under all three tiers --------------------------

func TestLimiterAllTiersSaturated(t *testing.T) {
	// All three tiers at 2.
	l := NewLimiter(2, 2, 2, 0)
	defer l.Close()

	// Saturate L3 (per-target) for ssh:h1.
	require.NoError(t, l.Acquire("ssh", "h1"))
	require.NoError(t, l.Acquire("ssh", "h1"))

	// Saturate L3 for ssh:h2 — but L1 (global=2) is already full.
	done := make(chan error, 1)
	go func() {
		done <- l.Acquire("ssh", "h2")
	}()

	select {
	case err := <-done:
		t.Fatalf("Acquire for h2 should block (global full): %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Release one h1 permit; h2 should now acquire.
	l.Release("ssh", "h1")
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(1 * time.Second):
		t.Fatal("Acquire for h2 did not succeed after Release")
	}

	// Clean up.
	l.Release("ssh", "h1")
	l.Release("ssh", "h2")
}

// --- Rollback on timeout does not leak permits ------------------------------

func TestLimiterNoLeakOnTimeout(t *testing.T) {
	// per-target=1, generous others, short timeout.
	l := NewLimiter(100, 100, 1, 30*time.Millisecond)
	defer l.Close()

	require.NoError(t, l.Acquire("ssh", "h1"))

	// This will time out at L3.
	err := l.Acquire("ssh", "h1")
	require.ErrorIs(t, err, ErrRateLimited)

	// After the timeout, no permits should be held for the failed acquire.
	// The successful acquire still holds its permits.
	stats := l.Stats()
	assert.Equal(t, 1, stats.GlobalInUse, "only the successful acquire should hold a global permit")
	assert.Equal(t, 1, stats.ChannelInUse["ssh"])
	assert.Equal(t, 1, stats.TargetInUse["ssh:h1"])

	// After releasing the successful acquire, a new acquire should succeed
	// immediately (proving no permits were leaked).
	l.Release("ssh", "h1")
	require.NoError(t, l.Acquire("ssh", "h1"))
	l.Release("ssh", "h1")
}
