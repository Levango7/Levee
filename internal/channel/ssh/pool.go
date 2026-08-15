// Package ssh pool implementation.
//
// SSHPool maintains a per-target (host:port) pool of *SSHChannel instances,
// providing the equivalent of OpenSSH ControlMaster multiplexing: the first
// caller to target a host pays the handshake cost; subsequent callers reuse
// the already-authenticated connection. Idle connections are reaped after a
// configurable idle timeout, and each target has a configurable concurrency
// cap to avoid overwhelming a single host.
//
// The pool is safe for concurrent use by multiple goroutines.
package ssh

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// PoolConfig governs the behaviour of an SSHPool.
type PoolConfig struct {
	// MaxPerTarget is the maximum number of concurrent connections the pool
	// will hand out for a single target host:port. Zero means use the
	// default (DefaultMaxPerTarget). A negative value disables the cap.
	MaxPerTarget int

	// IdleTimeout is how long an idle connection sits in the pool before
	// being reaped by the reaper. Zero means use the default
	// (DefaultIdleTimeout). A negative value disables reaping.
	IdleTimeout time.Duration

	// HealthCheckInterval is how often the reaper scans the pool for idle
	// connections to close. Zero means use the default
	// (DefaultHealthCheckInterval). A negative value disables reaping.
	HealthCheckInterval time.Duration
}

// DefaultMaxPerTarget is the default concurrency cap per target host:port.
const DefaultMaxPerTarget = 5

// DefaultIdleTimeout is the default idle reaping window.
const DefaultIdleTimeout = 5 * time.Minute

// DefaultHealthCheckInterval is the default reaper scan cadence.
const DefaultHealthCheckInterval = 30 * time.Second

// withDefaults returns a copy of cfg with zero fields replaced by defaults.
func (cfg PoolConfig) withDefaults() PoolConfig {
	out := cfg
	if out.MaxPerTarget == 0 {
		out.MaxPerTarget = DefaultMaxPerTarget
	}
	if out.IdleTimeout == 0 {
		out.IdleTimeout = DefaultIdleTimeout
	}
	if out.HealthCheckInterval == 0 {
		out.HealthCheckInterval = DefaultHealthCheckInterval
	}
	return out
}

// pooledConn wraps an SSHChannel with the bookkeeping the pool needs: the
// last time it was returned to the pool and whether it is currently checked
// out.
type pooledConn struct {
	channel  *SSHChannel
	lastUsed time.Time
	inUse    bool
}

// targetPool is the per-host:port state: a list of connections and a
// semaphore-like count of how many are currently checked out.
type targetPool struct {
	mu    sync.Mutex
	conns []*pooledConn
}

// SSHPool is a connection pool for SSH channels. It maintains one
// targetPool per host:port and a background reaper that closes idle
// connections older than IdleTimeout.
//
// Use NewPool to create a pool; the zero value is not ready to use.
type SSHPool struct {
	cfg     PoolConfig
	factory *SSHFactory

	mu      sync.Mutex
	targets map[string]*targetPool
	closed  bool

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewPool returns a new SSHPool with the given configuration. The pool
// starts its background reaper immediately. Use Close to shut it down.
func NewPool(cfg PoolConfig) *SSHPool {
	cfg = cfg.withDefaults()
	p := &SSHPool{
		cfg:     cfg,
		factory: &SSHFactory{},
		targets: make(map[string]*targetPool),
		stopCh:  make(chan struct{}),
	}
	// Only start the reaper when reaping is enabled.
	if cfg.IdleTimeout > 0 && cfg.HealthCheckInterval > 0 {
		p.wg.Add(1)
		go p.reaper()
	}
	return p
}

// Get returns a connected SSHChannel for the given target. If the pool has
// an idle, healthy connection for the target's host:port it is reused;
// otherwise a new channel is created and connected. Get blocks until either
// a connection is available or ctx is cancelled.
//
// When the per-target concurrency cap is reached Get blocks until a
// connection is returned via Put or ctx is cancelled.
func (p *SSHPool) Get(ctx context.Context, target channel.Target) (*SSHChannel, error) {
	if p.isClosed() {
		return nil, fmt.Errorf("ssh: pool is closed")
	}
	key := poolKey(target)
	tp := p.getTargetPool(key)

	for {
		// Try to grab an idle, healthy connection.
		if pc := p.tryAcquireIdle(tp); pc != nil {
			return pc.channel, nil
		}

		// No idle connection available. Check whether we can open a new one
		// without exceeding the per-target cap.
		if p.canOpenNew(tp) {
			ch, err := p.newAndConnect(ctx, target)
			if err != nil {
				p.releaseSlot(tp)
				return nil, err
			}
			p.registerConn(tp, ch)
			return ch, nil
		}

		// Cap reached: wait for a slot to free up or ctx to cancel.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
			// Retry the loop; an idle connection may have been returned in
			// the meantime.
		}
	}
}

// tryAcquireIdle finds an idle, healthy connection in tp, marks it in use and
// returns it. Returns nil when no idle healthy connection is available.
func (p *SSHPool) tryAcquireIdle(tp *targetPool) *pooledConn {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	now := time.Now()
	for _, pc := range tp.conns {
		if pc.inUse {
			continue
		}
		// Health check: drop dead connections.
		if !pc.channel.IsConnected() {
			_ = pc.channel.Close()
			pc.channel = nil
			continue
		}
		pc.inUse = true
		pc.lastUsed = now
		return pc
	}
	// Compact the slice to drop nilled-out entries.
	tp.conns = compact(tp.conns)
	return nil
}

// canOpenNew reports whether a new connection can be opened for tp without
// exceeding the per-target cap. It also reserves a slot when the answer is
// yes, so the caller must call releaseSlot on failure.
func (p *SSHPool) canOpenNew(tp *targetPool) bool {
	if p.cfg.MaxPerTarget < 0 {
		return true
	}
	tp.mu.Lock()
	defer tp.mu.Unlock()
	inUse := 0
	total := 0
	for _, pc := range tp.conns {
		if pc == nil {
			continue
		}
		total++
		if pc.inUse {
			inUse++
		}
	}
	if inUse >= p.cfg.MaxPerTarget {
		return false
	}
	// Reserve a slot by appending a placeholder in-use entry. The actual
	// channel is filled in by registerConn.
	tp.conns = append(tp.conns, &pooledConn{inUse: true})
	_ = total
	return true
}

// releaseSlot undoes the reservation made by canOpenNew when the subsequent
// newAndConnect fails. It removes the last in-use placeholder with no
// channel attached.
func (p *SSHPool) releaseSlot(tp *targetPool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for i := len(tp.conns) - 1; i >= 0; i-- {
		pc := tp.conns[i]
		if pc != nil && pc.inUse && pc.channel == nil {
			tp.conns = append(tp.conns[:i], tp.conns[i+1:]...)
			return
		}
	}
}

// registerConn attaches a freshly connected channel to the placeholder slot
// reserved by canOpenNew.
func (p *SSHPool) registerConn(tp *targetPool, ch *SSHChannel) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, pc := range tp.conns {
		if pc != nil && pc.inUse && pc.channel == nil {
			pc.channel = ch
			pc.lastUsed = time.Now()
			return
		}
	}
	// No placeholder found (should not happen): append a fresh entry.
	tp.conns = append(tp.conns, &pooledConn{channel: ch, inUse: true, lastUsed: time.Now()})
}

// newAndConnect creates a new SSHChannel for target and connects it.
func (p *SSHPool) newAndConnect(ctx context.Context, target channel.Target) (*SSHChannel, error) {
	ch, err := NewChannel(target, NewConfig())
	if err != nil {
		return nil, err
	}
	if err := ch.Connect(ctx); err != nil {
		return nil, err
	}
	return ch, nil
}

// Put returns a connection to the pool, marking it idle and available for
// reuse. Put is idempotent: calling it twice on the same channel is a no-op.
// If the channel is no longer healthy it is closed and dropped from the pool.
//
// After Put the caller must not use the channel again; the pool may hand it
// to another goroutine via Get.
func (p *SSHPool) Put(ch *SSHChannel) {
	if ch == nil {
		return
	}
	if p.isClosed() {
		_ = ch.Close()
		return
	}
	key := poolKey(ch.Target())
	tp := p.getTargetPool(key)
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, pc := range tp.conns {
		if pc != nil && pc.channel == ch {
			pc.inUse = false
			pc.lastUsed = time.Now()
			return
		}
	}
	// Channel not tracked by the pool (e.g. created outside). Close it to
	// avoid leaking the connection.
	_ = ch.Close()
}

// Close shuts down the pool, closing every connection it tracks and stopping
// the background reaper. Close is idempotent.
func (p *SSHPool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	close(p.stopCh)
	targets := p.targets
	p.targets = nil
	p.mu.Unlock()

	// Wait for the reaper to exit so we do not race it on connection close.
	p.wg.Wait()

	var lastErr error
	for _, tp := range targets {
		tp.mu.Lock()
		for _, pc := range tp.conns {
			if pc != nil && pc.channel != nil {
				if err := pc.channel.Close(); err != nil {
					lastErr = err
				}
			}
		}
		tp.conns = nil
		tp.mu.Unlock()
	}
	if lastErr != nil {
		return fmt.Errorf("ssh: pool close: %w", lastErr)
	}
	return nil
}

// Stats returns a snapshot of pool statistics for diagnostics.
type PoolStats struct {
	Targets    int
	TotalConns int
	IdleConns  int
	InUseConns int
}

// Stats returns a snapshot of the pool's current state. It is intended for
// diagnostics and tests; do not rely on it for synchronization.
func (p *SSHPool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	stats := PoolStats{Targets: len(p.targets)}
	for _, tp := range p.targets {
		tp.mu.Lock()
		for _, pc := range tp.conns {
			if pc == nil || pc.channel == nil {
				continue
			}
			stats.TotalConns++
			if pc.inUse {
				stats.InUseConns++
			} else {
				stats.IdleConns++
			}
		}
		tp.mu.Unlock()
	}
	return stats
}

// reaper is the background goroutine that periodically closes idle
// connections older than IdleTimeout. It exits when stopCh is closed.
func (p *SSHPool) reaper() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.HealthCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.reapIdle()
		}
	}
}

// reapIdle scans every target pool and closes connections that have been idle
// for longer than IdleTimeout.
func (p *SSHPool) reapIdle() {
	now := time.Now()
	p.mu.Lock()
	targets := make([]*targetPool, 0, len(p.targets))
	for _, tp := range p.targets {
		targets = append(targets, tp)
	}
	p.mu.Unlock()

	for _, tp := range targets {
		tp.mu.Lock()
		for i, pc := range tp.conns {
			if pc == nil || pc.inUse || pc.channel == nil {
				continue
			}
			if now.Sub(pc.lastUsed) > p.cfg.IdleTimeout {
				_ = pc.channel.Close()
				tp.conns[i] = nil
			}
		}
		tp.conns = compact(tp.conns)
		tp.mu.Unlock()
	}
}

// isClosed reports whether the pool has been closed.
func (p *SSHPool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// getTargetPool returns the targetPool for key, creating it if necessary.
func (p *SSHPool) getTargetPool(key string) *targetPool {
	p.mu.Lock()
	defer p.mu.Unlock()
	tp, ok := p.targets[key]
	if !ok {
		tp = &targetPool{}
		p.targets[key] = tp
	}
	return tp
}

// poolKey returns the host:port key used to bucket connections for a target.
func poolKey(target channel.Target) string {
	port := target.Port()
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("%s:%d", target.Host(), port)
}

// compact removes nil entries from s in place and returns the shortened slice.
func compact(s []*pooledConn) []*pooledConn {
	out := s[:0]
	for _, v := range s {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}
