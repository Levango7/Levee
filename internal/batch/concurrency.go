// concurrency.go implements the per-batch ConcurrencyManager (design
// doc section 4.4, MVP task W5-T026). It wraps a *channel.Limiter (T015)
// to provide a batch-local concurrency gate with first-class context
// support, a configurable wait timeout, and lightweight statistics.
//
// When the underlying limiter is saturated, Acquire blocks
// (back-pressure) instead of rejecting the caller; a wait timeout
// converts a stuck Acquire into ErrAcquireTimeout so the caller can
// shed load gracefully. On success Acquire returns a ReleaseFunc that
// the caller MUST call exactly once to release the permit; the
// ReleaseFunc is idempotent so a defensive double-call is harmless.
//
// The ConcurrencyManager is safe for concurrent use by multiple
// goroutines.
package batch

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// ErrAcquireTimeout is returned by ConcurrencyManager.Acquire when the
// caller waited longer than the configured wait timeout without
// obtaining a permit. Callers should treat this as a back-pressure
// signal and retry later (or shed the request).
var ErrAcquireTimeout = errors.New("batch: concurrency acquire timed out")

// ErrManagerClosed is returned by ConcurrencyManager.Acquire after
// Close has been called.
var ErrManagerClosed = errors.New("batch: concurrency manager is closed")

// ReleaseFunc releases a permit previously acquired by
// ConcurrencyManager.Acquire. It is idempotent: calling it more than
// once is a no-op. The caller MUST call it exactly once for each
// successful Acquire.
type ReleaseFunc func()

// ConcurrencyManager wraps a *channel.Limiter to provide a batch-local
// concurrency gate with context support, wait timeout, and statistics.
// The zero value is not usable — use NewConcurrencyManager.
//
// A ConcurrencyManager is configured once at construction time and may
// then be used by any number of batches sequentially or concurrently.
// The underlying *channel.Limiter must itself be concurrency-safe (it
// is).
type ConcurrencyManager struct {
	limiter     *channel.Limiter
	channelType string
	waitTimeout time.Duration

	// Atomic flag set by Close.
	closed int32

	// Atomic counters for diagnostics.
	inFlight      int64 // current number of held permits
	waiting       int64 // current number of Acquire calls blocked in the limiter
	totalAcquired int64 // cumulative successful Acquire calls
	totalTimedOut int64 // cumulative Acquire calls that returned an error
}

// ConcurrencyStats is a snapshot of the ConcurrencyManager's state at a
// point in time. It is intended for diagnostics (e.g. `levee debug
// batch`) and must not be used for synchronization.
type ConcurrencyStats struct {
	// InFlight is the current number of held permits (Acquire succeeded
	// but the matching ReleaseFunc has not yet been called).
	InFlight int64

	// Waiting is the current number of Acquire calls blocked inside the
	// underlying limiter.
	Waiting int64

	// TotalAcquired is the cumulative number of successful Acquire calls
	// since the manager was created.
	TotalAcquired int64

	// TotalTimedOut is the cumulative number of Acquire calls that
	// returned an error (timeout, cancellation, or closed manager).
	TotalTimedOut int64
}

// NewConcurrencyManager returns a ConcurrencyManager that wraps the
// given limiter. The channelType is used as the L2 key when calling the
// limiter (typically "batch"); targets are passed through as the L3
// key. A waitTimeout of zero means Acquire never times out on its own
// (callers must rely on ctx to bound the wait).
//
// A nil limiter is treated as "unlimited": Acquire always succeeds
// immediately and the ReleaseFunc is a no-op. This is useful for tests
// and for configurations where only the batch's MaxConcurrency matters.
func NewConcurrencyManager(limiter *channel.Limiter, channelType string, waitTimeout time.Duration) *ConcurrencyManager {
	return &ConcurrencyManager{
		limiter:     limiter,
		channelType: channelType,
		waitTimeout: waitTimeout,
	}
}

// Acquire blocks until a permit is available for the given target or
// one of the following occurs:
//   - ctx is cancelled: returns an error wrapping ctx.Err().
//   - the wait timeout elapses: returns ErrAcquireTimeout.
//   - the manager is closed: returns ErrManagerClosed.
//
// On success Acquire returns a ReleaseFunc and a nil error. The caller
// MUST call the ReleaseFunc exactly once to release the permit; the
// ReleaseFunc is idempotent so a defensive double-call is harmless.
//
// When Acquire returns an error the corresponding ReleaseFunc is nil
// and the caller must not call it. When the underlying limiter is
// saturated Acquire blocks (back-pressure) instead of rejecting the
// caller; the wait timeout (or ctx) converts a stuck Acquire into an
// error so the caller can shed load gracefully.
func (m *ConcurrencyManager) Acquire(ctx context.Context, target string) (ReleaseFunc, error) {
	if atomic.LoadInt32(&m.closed) != 0 {
		atomic.AddInt64(&m.totalTimedOut, 1)
		return nil, ErrManagerClosed
	}

	if err := ctx.Err(); err != nil {
		atomic.AddInt64(&m.totalTimedOut, 1)
		return nil, fmt.Errorf("batch: acquire cancelled before start: %w", err)
	}

	// Unlimited fast path: no limiter, just count.
	if m.limiter == nil {
		atomic.AddInt64(&m.totalAcquired, 1)
		atomic.AddInt64(&m.inFlight, 1)
		var released int32
		return func() {
			if atomic.CompareAndSwapInt32(&released, 0, 1) {
				atomic.AddInt64(&m.inFlight, -1)
			}
		}, nil
	}

	atomic.AddInt64(&m.waiting, 1)
	defer atomic.AddInt64(&m.waiting, -1)

	// The underlying limiter.Acquire does not accept a context, so we
	// run it in a goroutine and select between its result, ctx.Done(),
	// and the wait timeout. When ctx or the timeout fires first we
	// detach a rescue goroutine that drains the result channel and
	// releases the permit if the limiter eventually granted one, so no
	// slot is leaked.
	type acquireResult struct{ err error }
	resultCh := make(chan acquireResult, 1)
	go func() {
		resultCh <- acquireResult{err: m.limiter.Acquire(m.channelType, target)}
	}()

	var timeoutCh <-chan time.Time
	if m.waitTimeout > 0 {
		timer := time.NewTimer(m.waitTimeout)
		defer timer.Stop()
		timeoutCh = timer.C
	}

	select {
	case res := <-resultCh:
		if res.err != nil {
			atomic.AddInt64(&m.totalTimedOut, 1)
			return nil, fmt.Errorf("batch: limiter acquire failed: %w", res.err)
		}
		atomic.AddInt64(&m.totalAcquired, 1)
		atomic.AddInt64(&m.inFlight, 1)
		var released int32
		return func() {
			if atomic.CompareAndSwapInt32(&released, 0, 1) {
				atomic.AddInt64(&m.inFlight, -1)
				m.limiter.Release(m.channelType, target)
			}
		}, nil
	case <-ctx.Done():
		// The background goroutine is still waiting on the limiter.
		// When it eventually acquires a permit, release it immediately
		// so we don't leak a slot.
		go func() {
			res := <-resultCh
			if res.err == nil {
				m.limiter.Release(m.channelType, target)
			}
		}()
		atomic.AddInt64(&m.totalTimedOut, 1)
		return nil, fmt.Errorf("batch: acquire cancelled: %w", ctx.Err())
	case <-timeoutCh:
		go func() {
			res := <-resultCh
			if res.err == nil {
				m.limiter.Release(m.channelType, target)
			}
		}()
		atomic.AddInt64(&m.totalTimedOut, 1)
		return nil, ErrAcquireTimeout
	}
}

// Stats returns a snapshot of the manager's current state. The snapshot
// is best-effort: the individual counters are read atomically but may
// be slightly stale by the time the caller reads them.
func (m *ConcurrencyManager) Stats() ConcurrencyStats {
	return ConcurrencyStats{
		InFlight:      atomic.LoadInt64(&m.inFlight),
		Waiting:       atomic.LoadInt64(&m.waiting),
		TotalAcquired: atomic.LoadInt64(&m.totalAcquired),
		TotalTimedOut: atomic.LoadInt64(&m.totalTimedOut),
	}
}

// Close marks the manager as closed. Subsequent Acquire calls return
// ErrManagerClosed. In-flight permits are not revoked; callers that
// currently hold permits must still call their ReleaseFunc. Close is
// idempotent and safe for concurrent use.
func (m *ConcurrencyManager) Close() {
	atomic.StoreInt32(&m.closed, 1)
}
