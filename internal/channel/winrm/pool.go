package winrm

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// --- PoolConfig -------------------------------------------------------------

// PoolConfig configures a WinRMPool.
type PoolConfig struct {
	// MaxConcurrentPerTarget bounds how many channels may be checked out
	// simultaneously for a single target (host:port). Zero means the default
	// of 3. WinRM concurrency on a single host is weak (each command opens a
	// separate shell and consumes server resources), so the default is
	// intentionally low.
	MaxConcurrentPerTarget int

	// ConnectTimeout bounds the Connect call when the pool must create a new
	// channel. Zero means no explicit timeout (the channel Config.Timeout
	// still applies at the TCP level).
	ConnectTimeout time.Duration

	// DisableHealthCheck, when true, skips the IsConnected verification that
	// Get otherwise performs on each idle channel before handing it out. The
	// default (false) enables health checks so that stale connections are
	// discarded and replaced automatically.
	DisableHealthCheck bool
}

// withDefaults returns a copy of p with zero values replaced by defaults.
func (p PoolConfig) withDefaults() PoolConfig {
	out := p
	if out.MaxConcurrentPerTarget <= 0 {
		out.MaxConcurrentPerTarget = 3
	}
	return out
}

// healthCheckEnabled reports whether Get should verify idle channels.
func (p PoolConfig) healthCheckEnabled() bool {
	return !p.DisableHealthCheck
}

// poolKey returns the pool bucket key for a target: host:port plus username.
// Channels are only reused when both the endpoint AND the credential identity
// match: two different usernames pointed at the same host:port must never
// share a pooled client, or one caller would execute commands under another
// caller's authenticated session (credential aliasing).
func poolKey(t channel.Target) string {
	port := t.Port()
	if port == 0 {
		port = DefaultHTTPPort
	}
	return fmt.Sprintf("%s:%d|%s", t.Host(), port, t.Credentials().Username)
}

// --- targetPool -------------------------------------------------------------

// targetPool manages the idle queue + concurrency semaphore for a single
// target host:port.
type targetPool struct {
	target channel.Target
	max    int

	// sem is a counting semaphore: acquiring it grants the right to use one
	// channel for this target. Its capacity is max.
	sem chan struct{}

	// idle is a buffered channel holding channels that have been Put back and
	// are ready for reuse. Its capacity is max (a target can never have more
	// than max live channels because sem bounds concurrency).
	idle chan *WinRMChannel

	// created counts how many channels have been created for this bucket and
	// are still alive (checked out + idle). Used for diagnostics and to verify
	// Close drains everything.
	mu      sync.Mutex
	created int
}

func newTargetPool(target channel.Target, max int) *targetPool {
	return &targetPool{
		target: target,
		max:    max,
		sem:    make(chan struct{}, max),
		idle:   make(chan *WinRMChannel, max),
	}
}

// acquireSem blocks until a concurrency slot is available or ctx is cancelled.
func (tp *targetPool) acquireSem(ctx context.Context) error {
	select {
	case tp.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// releaseSem returns a concurrency slot.
func (tp *targetPool) releaseSem() {
	<-tp.sem
}

// putIdle returns a channel to the idle queue. If the queue is full (which
// should not happen because sem bounds the live count) the channel is closed.
func (tp *targetPool) putIdle(ch *WinRMChannel) {
	select {
	case tp.idle <- ch:
		// returned to idle queue
	default:
		// idle queue full; close the channel
		_ = ch.Close()
		tp.mu.Lock()
		tp.created--
		tp.mu.Unlock()
	}
}

// getIdle returns an idle channel if one is available, else nil.
func (tp *targetPool) getIdle() *WinRMChannel {
	select {
	case ch := <-tp.idle:
		return ch
	default:
		return nil
	}
}

// --- WinRMPool --------------------------------------------------------------

// WinRMPool is a connection pool for WinRM channels. It maintains a per-target
// (host:port) bucket of reusable channels with a configurable concurrency
// cap, implementing the "connection pool + single-connection-single-command"
// strategy mandated by docs/levee-design.md §4.1.3.
//
// Get returns a connected channel; Put returns it to the pool. Channels that
// fail the health check on Get are discarded and replaced with a fresh one.
// Close drains and closes every channel in every bucket.
//
// All methods are safe for concurrent use by multiple goroutines.
type WinRMPool struct {
	factory *WinRMFactory
	cfg     PoolConfig

	// createFunc builds and connects a channel for the given target. It is
	// initialised in NewPool to the factory-based implementation and exists
	// as an exported-but-internal hook so that white-box tests can inject
	// fake channels without a real winrm.Client.
	createFunc func(ctx context.Context, target channel.Target) (*WinRMChannel, error)

	mu     sync.Mutex
	pools  map[string]*targetPool
	closed bool
}

// NewPool returns a WinRMPool that creates channels via f and enforces cfg on
// each target bucket. If f is nil, a default factory is used.
func NewPool(f *WinRMFactory, cfg PoolConfig) *WinRMPool {
	if f == nil {
		f = NewFactory(Config{})
	}
	cfg = cfg.withDefaults()
	p := &WinRMPool{
		factory: f,
		cfg:     cfg,
		pools:   make(map[string]*targetPool),
	}
	p.createFunc = p.createAndConnect
	return p
}

// bucket returns the targetPool for the given target, creating one if needed.
// It returns an error if the pool is closed.
func (p *WinRMPool) bucket(target channel.Target) (*targetPool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errPoolClosed
	}
	key := poolKey(target)
	tp, ok := p.pools[key]
	if !ok {
		tp = newTargetPool(target, p.cfg.MaxConcurrentPerTarget)
		p.pools[key] = tp
	}
	return tp, nil
}

// Get returns a connected WinRMChannel for the given target. It blocks until
// a concurrency slot is available (bounded by MaxConcurrentPerTarget) and
// then either reuses an idle channel or creates a new one.
//
// If ctx is cancelled before a channel is available, Get returns ctx.Err().
// If the pool is closed, Get returns errPoolClosed.
//
// The caller MUST call Put when done with the channel; failing to do so leaks
// a concurrency slot. Executing a single command and then Put-ing is the
// intended "single-connection-single-command" usage pattern.
func (p *WinRMPool) Get(ctx context.Context, target channel.Target) (*WinRMChannel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tp, err := p.bucket(target)
	if err != nil {
		return nil, err
	}

	// Acquire a concurrency slot (bounds in-flight channels for this target).
	if err := tp.acquireSem(ctx); err != nil {
		return nil, err
	}

	// Now we own a slot. Try to grab an idle channel; if none, create one.
	// On any failure after acquiring the slot we must release it.
	for {
		if err := ctx.Err(); err != nil {
			tp.releaseSem()
			return nil, err
		}

		ch := tp.getIdle()
		if ch == nil {
			// No idle channel: create a new one.
			newCh, createErr := p.createFunc(ctx, target)
			if createErr != nil {
				tp.releaseSem()
				return nil, createErr
			}
			tp.mu.Lock()
			tp.created++
			tp.mu.Unlock()
			return newCh, nil
		}

		// Got an idle channel: health-check it if enabled.
		if p.cfg.healthCheckEnabled() && !ch.IsConnected() {
			// Unhealthy: discard and try again (we still hold the slot).
			_ = ch.Close()
			tp.mu.Lock()
			tp.created--
			tp.mu.Unlock()
			continue
		}
		return ch, nil
	}
}

// createAndConnect builds a new channel via the factory and connects it.
func (p *WinRMPool) createAndConnect(ctx context.Context, target channel.Target) (*WinRMChannel, error) {
	raw, err := p.factory.Create(target)
	if err != nil {
		return nil, fmt.Errorf("winrm pool: create channel: %w", err)
	}
	ch, ok := raw.(*WinRMChannel)
	if !ok {
		// The factory always returns *WinRMChannel; this is a defensive guard.
		_ = raw.Close()
		return nil, fmt.Errorf("winrm pool: factory returned %T, want *WinRMChannel", raw)
	}

	connectCtx := ctx
	if p.cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, p.cfg.ConnectTimeout)
		defer cancel()
	}
	if err := ch.Connect(connectCtx); err != nil {
		_ = ch.Close()
		return nil, fmt.Errorf("winrm pool: connect: %w", err)
	}
	return ch, nil
}

// Put returns a channel to the pool. The channel becomes available for reuse
// by subsequent Get calls for the same target. Put is idempotent in the sense
// that calling it on a pool that is closed simply closes the channel.
//
// After Put the caller must not use the channel again.
func (p *WinRMPool) Put(ch *WinRMChannel) {
	if ch == nil {
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = ch.Close()
		return
	}
	key := poolKey(ch.Target())
	tp, ok := p.pools[key]
	p.mu.Unlock()

	if !ok {
		// Bucket vanished (can only happen if Close raced and removed it).
		_ = ch.Close()
		return
	}

	tp.putIdle(ch)
	tp.releaseSem()
}

// Close closes all channels in all buckets and marks the pool closed. After
// Close, Get returns errPoolClosed and Put closes the channel. Close is
// idempotent and safe for concurrent use.
func (p *WinRMPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	pools := p.pools
	p.pools = nil
	p.mu.Unlock()

	for _, tp := range pools {
		// Drain idle channels and close them.
		draining := true
		for draining {
			select {
			case ch := <-tp.idle:
				_ = ch.Close()
				tp.mu.Lock()
				tp.created--
				tp.mu.Unlock()
			default:
				draining = false
			}
		}
	}
	return nil
}

// Stats returns a snapshot of pool statistics per target key. Intended for
// diagnostics (e.g. `levee debug pool`).
type PoolStats struct {
	Target string
	Max    int
	Idle   int
	InUse  int
}

// Stats returns per-target pool statistics. The snapshot is best-effort and
// may be slightly stale by the time it is returned.
func (p *WinRMPool) Stats() []PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PoolStats, 0, len(p.pools))
	for key, tp := range p.pools {
		tp.mu.Lock()
		created := tp.created
		tp.mu.Unlock()
		idle := len(tp.idle)
		out = append(out, PoolStats{
			Target: key,
			Max:    tp.max,
			Idle:   idle,
			InUse:  created - idle,
		})
	}
	return out
}

// errPoolClosed is returned by Get when the pool has been closed.
var errPoolClosed = errors.New("winrm pool: closed")
