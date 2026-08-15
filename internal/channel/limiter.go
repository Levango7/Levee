// Package channel limiter implementation.
//
// The Limiter enforces three concurrency tiers to protect both the LEVEE
// engine and the remote targets from being overwhelmed:
//
//	L1 — global concurrency cap (across all channels and targets).
//	L2 — per-channel-type cap (e.g. at most 20 concurrent SSH sessions).
//	L3 — per-target cap (e.g. at most 5 concurrent operations on one host).
//
// Acquire blocks (back-pressure) when a tier is saturated, instead of
// rejecting the caller. A configurable timeout converts a stuck Acquire into
// ErrRateLimited so the upper layers can shed load gracefully.
//
// The Limiter is safe for concurrent use by multiple goroutines.
package channel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRateLimited is returned by Limiter.Acquire when the caller waited longer
// than the configured timeout without obtaining a permit. Callers should treat
// this as a back-pressure signal and retry later (or shed the request).
var ErrRateLimited = errors.New("channel: rate limited (acquire timed out)")

// ErrLimiterClosed is returned by Limiter.Acquire after Close has been called.
var ErrLimiterClosed = errors.New("channel: limiter is closed")

// semaphore is a counting semaphore backed by a buffered channel. Sending a
// token acquires a permit; receiving a token releases it.
type semaphore struct {
	ch  chan struct{}
	max int
}

func newSemaphore(max int) *semaphore {
	return &semaphore{
		ch:  make(chan struct{}, max),
		max: max,
	}
}

// acquire blocks until a permit is available or ctx is cancelled. It returns
// nil on success and ctx.Err() on cancellation.
func (s *semaphore) acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release returns a permit to the semaphore. It is non-blocking; if the
// semaphore is already full (which should never happen when acquire/release
// are paired correctly) the call panics to surface the bug.
func (s *semaphore) release() {
	select {
	case <-s.ch:
	default:
		panic("channel: semaphore release without matching acquire")
	}
}

// inUse returns the current number of acquired permits.
func (s *semaphore) inUse() int { return len(s.ch) }

// Limiter enforces three concurrency tiers (global, per-channel-type,
// per-target) with back-pressure and timeout. The zero value is not usable;
// use NewLimiter.
type Limiter struct {
	globalMax     int
	perChannelMax int
	perTargetMax  int
	timeout       time.Duration

	global *semaphore

	// perChannel and perTarget hold semaphores keyed by channel type and
	// "channelType:targetID" respectively. They are created lazily on first
	// Acquire for a given key.
	perChannel sync.Map // map[string]*semaphore
	perTarget  sync.Map // map[string]*semaphore

	// mu guards the closed flag. The semaphores themselves are concurrency-safe.
	mu     sync.RWMutex
	closed bool

	// Atomic counters for diagnostics.
	totalAcquired int64
	totalTimedOut int64
}

// LimiterStats is a snapshot of the limiter's state at a point in time. It is
// intended for diagnostics (e.g. `levee debug limiter`) and must not be used
// for synchronization.
type LimiterStats struct {
	// GlobalInUse is the current number of globally held permits.
	GlobalInUse int
	// GlobalMax is the global concurrency cap.
	GlobalMax int

	// ChannelInUse maps each channel type to its current in-use count.
	ChannelInUse map[string]int
	// ChannelMax is the per-channel-type concurrency cap.
	ChannelMax int

	// TargetInUse maps each "channelType:targetID" to its current in-use count.
	TargetInUse map[string]int
	// TargetMax is the per-target concurrency cap.
	TargetMax int

	// TotalAcquired is the cumulative number of successful Acquire calls.
	TotalAcquired int64
	// TotalTimedOut is the cumulative number of Acquire calls that returned
	// ErrRateLimited.
	TotalTimedOut int64
}

// NewLimiter returns a Limiter with the given concurrency caps and acquire
// timeout. A timeout of zero means Acquire never times out (pure back-pressure,
// callers must rely on ctx to bound the wait).
//
// Any cap of zero or negative is treated as "unlimited" for that tier: the
// semaphore is still tracked but never blocks. At least one tier should be
// finite to make the limiter meaningful.
func NewLimiter(global, perChannel, perTarget int, timeout time.Duration) *Limiter {
	return &Limiter{
		globalMax:     global,
		perChannelMax: perChannel,
		perTargetMax:  perTarget,
		timeout:       timeout,
		global:        newSemaphore(effectiveCap(global)),
	}
}

// effectiveCap returns max when it is positive, or a very large number when
// the cap is zero/negative (effectively unlimited). We use a large but finite
// buffer so that the semaphore's len() still works for stats.
func effectiveCap(max int) int {
	if max <= 0 {
		// Effectively unlimited. Use a large number that is unlikely to be
		// reached in practice; if it ever is, the limiter degrades to
		// back-pressure, which is still correct.
		return 1 << 20 // 1,048,576
	}
	return max
}

// targetKey returns the map key for a (channelType, targetID) pair.
func targetKey(channelType, targetID string) string {
	return channelType + ":" + targetID
}

// getOrCreateChannelSem returns the per-channel semaphore for channelType,
// creating it on first use.
func (l *Limiter) getOrCreateChannelSem(channelType string) *semaphore {
	if v, ok := l.perChannel.Load(channelType); ok {
		return v.(*semaphore)
	}
	s := newSemaphore(effectiveCap(l.perChannelMax))
	actual, loaded := l.perChannel.LoadOrStore(channelType, s)
	if loaded {
		return actual.(*semaphore)
	}
	return s
}

// getOrCreateTargetSem returns the per-target semaphore for the given
// channelType and targetID, creating it on first use.
func (l *Limiter) getOrCreateTargetSem(channelType, targetID string) *semaphore {
	key := targetKey(channelType, targetID)
	if v, ok := l.perTarget.Load(key); ok {
		return v.(*semaphore)
	}
	s := newSemaphore(effectiveCap(l.perTargetMax))
	actual, loaded := l.perTarget.LoadOrStore(key, s)
	if loaded {
		return actual.(*semaphore)
	}
	return s
}

// Acquire blocks until a permit is available at all three tiers (L3 per-target,
// L2 per-channel, L1 global) or the configured timeout elapses. On success the
// caller MUST call Release with the same arguments to return the permits.
//
// Acquire acquires in order L3 -> L2 -> L1 and rolls back on failure so that
// no permits are leaked. When the limiter is closed, Acquire returns
// ErrLimiterClosed immediately.
func (l *Limiter) Acquire(channelType, targetID string) error {
	l.mu.RLock()
	if l.closed {
		l.mu.RUnlock()
		return ErrLimiterClosed
	}
	l.mu.RUnlock()

	ctx := context.Background()
	var cancel context.CancelFunc
	if l.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, l.timeout)
		defer cancel()
	}

	// L3: per-target.
	l3 := l.getOrCreateTargetSem(channelType, targetID)
	if err := l3.acquire(ctx); err != nil {
		atomic.AddInt64(&l.totalTimedOut, 1)
		return ErrRateLimited
	}

	// L2: per-channel.
	l2 := l.getOrCreateChannelSem(channelType)
	if err := l2.acquire(ctx); err != nil {
		l3.release()
		atomic.AddInt64(&l.totalTimedOut, 1)
		return ErrRateLimited
	}

	// L1: global.
	if err := l.global.acquire(ctx); err != nil {
		l2.release()
		l3.release()
		atomic.AddInt64(&l.totalTimedOut, 1)
		return ErrRateLimited
	}

	atomic.AddInt64(&l.totalAcquired, 1)
	return nil
}

// Release returns the permits held for the given (channelType, targetID) pair.
// Release must be called exactly once for each successful Acquire; calling it
// without a matching Acquire panics to surface the bug.
//
// Release releases in order L1 -> L2 -> L3 (the reverse of Acquire) so that
// the global slot is freed first, giving other targets a chance to proceed.
func (l *Limiter) Release(channelType, targetID string) {
	// L1: global.
	l.global.release()

	// L2: per-channel.
	if v, ok := l.perChannel.Load(channelType); ok {
		v.(*semaphore).release()
	}

	// L3: per-target.
	key := targetKey(channelType, targetID)
	if v, ok := l.perTarget.Load(key); ok {
		v.(*semaphore).release()
	}
}

// Stats returns a snapshot of the limiter's current state. The snapshot is
// best-effort: the per-channel and per-target maps are populated atomically
// but the individual in-use counts may be slightly stale by the time the
// caller reads them.
func (l *Limiter) Stats() LimiterStats {
	stats := LimiterStats{
		GlobalInUse:   l.global.inUse(),
		GlobalMax:     l.globalMax,
		ChannelInUse:  make(map[string]int),
		ChannelMax:    l.perChannelMax,
		TargetInUse:   make(map[string]int),
		TargetMax:     l.perTargetMax,
		TotalAcquired: atomic.LoadInt64(&l.totalAcquired),
		TotalTimedOut: atomic.LoadInt64(&l.totalTimedOut),
	}
	l.perChannel.Range(func(k, v any) bool {
		stats.ChannelInUse[k.(string)] = v.(*semaphore).inUse()
		return true
	})
	l.perTarget.Range(func(k, v any) bool {
		stats.TargetInUse[k.(string)] = v.(*semaphore).inUse()
		return true
	})
	return stats
}

// Close marks the limiter as closed. Subsequent Acquire calls return
// ErrLimiterClosed. In-flight permits are not revoked; callers that currently
// hold permits must still call Release. Close is idempotent.
func (l *Limiter) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
}
