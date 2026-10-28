// pool.go is the cross-run half of the channel plugin's session story (D-1c).
//
// The run executor keeps one channel per target for the life of a run
// (internal/wiring/exec.go channelFor), so a new run always re-dials. For a
// built-in transport that is cheap. For a channel plugin the dial IS the
// expensive, stateful part — a NETCONF hello, a bastion jump, a vendor
// handshake — and the plugin's resident process is what carries that state.
// This pool hands a live session to a later run instead of paying for the dial
// again.
//
// # What is never shared, and why the key says so explicitly
//
// A pooled session is keyed by (plugin, host, port, type, credential
// fingerprint). Every component is load-bearing:
//
//   - credential fingerprint, not the reference NAME: two inventory rows can
//     name the same credential but resolve to different secrets (or the same
//     name under two tenants). Sharing by name would let one run act as
//     another's subject — the exact failure the plaintext boundary in
//     channelConnectParams exists to prevent. The fingerprint is host-side
//     only; nothing about it reaches the plugin.
//   - host/port/type: the session is addressed to one target. Guessing that
//     two rows are "the same place" is not this layer's call.
//
// A session marked dead is never handed out: the session contract is "a failed
// or abandoned request kills the stream", so Alive() is re-checked on every
// acquire and dead entries are dropped on sight. A process that exits during an
// idle stretch is the one death the host cannot see without a request (the
// protocol has no heartbeat): the session keeps reading Alive() until the first
// reuse fails, which kills it exactly like any other failed request. The
// failure is reported, never read as success, and the next run dials fresh.
//
// # Bounds
//
// All three exist because any one alone leaves a leak:
//
//   - maxIdleSessions caps how many idle sessions one plugin process keeps;
//     publishing one more evicts the oldest idle entry.
//   - idleSessionTTL reaps a session that stopped being used.
//   - maxSessionAge reaps an idle session that has simply been open too long
//     (remote state accumulates; a plugin may cap connection age behind our
//     back).
//
// A session with a live lease (refs > 0) is NEVER reaped — pulling a session
// out from under an in-flight run would turn a pool miss into a failed run.
// Age therefore only applies once the session is idle; a chronically busy
// session is kept until it goes idle.
package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

const (
	// maxIdleSessions bounds one plugin's idle pool. Eight covers the usual
	// "a handful of devices per environment" shape without letting an
	// inventory of hundreds pin that many processes open.
	maxIdleSessions = 8
	// idleSessionTTL reaps a session nobody used for this long. Two minutes
	// covers the gap between two runs in a batch while still letting an
	// operator who stops using a plugin see its processes go.
	idleSessionTTL = 2 * time.Minute
	// maxSessionAge caps how long an idle session may stay pooled. A session
	// opened half an hour ago may have accumulated remote state the next run
	// did not create; re-dialling is the honest default after that.
	maxSessionAge = 30 * time.Minute
	// poolSweepInterval is how often the background reaper runs. Release also
	// reaps, so this only bounds how long an idle session can outlive its TTL
	// when nothing else happens.
	poolSweepInterval = 30 * time.Second
)

// poolSession is what the pool stores and reaps. It is also what the channel
// adapter talks to, so the interface carries Request; tests implement it with
// a fake and assert the pool's own rules without spawning processes.
type poolSession interface {
	Alive() bool
	Request(ctx context.Context, req HostRequest) (PluginResponse, error)
	// teardown ends the session for good: the target-level goodbye, then the
	// process reap. It is the path Close used to take before pooling.
	teardown()
}

// poolKey is the sharing decision made explicit. See the package comment.
type poolKey struct {
	plugin string
	host   string
	port   int
	typ    string
	credFP string // credential fingerprint; host-side only
}

type poolEntry struct {
	sess     poolSession
	refs     int       // leases handed out (0 = idle)
	opened   time.Time // when this session was dialled
	idleFrom time.Time // zero while refs > 0
}

// sessionPool is a per-plugin pool of live channel sessions. The zero value is
// not usable; construct with newSessionPool.
type sessionPool struct {
	mu      sync.Mutex
	entries map[poolKey]*poolEntry
	closed  bool

	// now is injectable so the age/TTL rules can be tested without sleeping.
	now func() time.Time

	maxIdle int
	idleTTL time.Duration
	maxAge  time.Duration
}

func newSessionPool() *sessionPool {
	return &sessionPool{
		entries: make(map[poolKey]*poolEntry),
		now:     time.Now,
		maxIdle: maxIdleSessions,
		idleTTL: idleSessionTTL,
		maxAge:  maxSessionAge,
	}
}

// acquire hands out the pooled session for key with one lease. It returns
// (nil, false) when there is none — the caller must dial. Dead entries, and
// idle entries past their TTL/age, are torn down here rather than returned.
func (p *sessionPool) acquire(key poolKey) (poolSession, bool) {
	p.mu.Lock()
	e, ok := p.entries[key]
	if !ok {
		p.mu.Unlock()
		return nil, false
	}
	if !e.sess.Alive() || (e.refs == 0 && p.expired(e, p.now())) {
		delete(p.entries, key)
		p.mu.Unlock()
		e.sess.teardown()
		return nil, false
	}
	e.refs++
	e.idleFrom = time.Time{}
	p.mu.Unlock()
	return e.sess, true
}

// publish records a freshly dialled session as in use by the caller (one
// lease). If another dial won the race to publish the same key, the caller's
// session is torn down and the winner is returned with a lease instead —
// exactly one session survives per key, and no caller is left holding a
// session the pool does not know about.
func (p *sessionPool) publish(key poolKey, sess poolSession) (poolSession, bool) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		sess.teardown()
		return nil, false
	}
	if e, ok := p.entries[key]; ok && e.sess != sess {
		e.refs++
		e.idleFrom = time.Time{}
		p.mu.Unlock()
		sess.teardown()
		return e.sess, true
	}
	p.entries[key] = &poolEntry{sess: sess, refs: 1, opened: p.now()}
	victims := p.trimIdleLocked()
	p.mu.Unlock()
	for _, victim := range victims {
		victim.sess.teardown()
	}
	return sess, true
}

// release returns one lease. The session stays pooled when that was the last
// lease; if the pool has been shut down (or the entry was evicted while this
// lease was held) the caller's session is torn down now.
func (p *sessionPool) release(key poolKey, sess poolSession) {
	var (
		evicted bool
		closed  bool
		dead    bool
	)
	p.mu.Lock()
	e, ok := p.entries[key]
	closed = p.closed
	switch {
	case !ok || e.sess != sess:
		evicted = true
	case e.refs > 0:
		e.refs--
		if e.refs == 0 {
			e.idleFrom = p.now()
		}
		if !e.sess.Alive() {
			dead = true
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()

	if evicted || closed || dead {
		sess.teardown()
		return
	}
	p.mu.Lock()
	victims := p.trimIdleLocked()
	p.mu.Unlock()
	for _, victim := range victims {
		victim.sess.teardown()
	}
	p.sweep()
}

// run reaps on a ticker until ctx ends, then shuts the pool down. Serve's
// lifecycle context drives it, so a shutdown also kills every idle session.
func (p *sessionPool) run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = poolSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.shutdown()
			return
		case <-ticker.C:
			p.sweep()
		}
	}
}

// sweep reaps entries that outlived their TTL or age.
func (p *sessionPool) sweep() {
	p.mu.Lock()
	var doomed []*poolEntry
	now := p.now()
	for key, e := range p.entries {
		if e.refs > 0 {
			continue // in use; release re-checks
		}
		if p.expired(e, now) {
			doomed = append(doomed, e)
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()
	for _, e := range doomed {
		e.sess.teardown()
	}
}

// expired reports whether an idle entry is past its TTL or age. Callers filter
// on refs first: an in-use entry is never expired.
func (p *sessionPool) expired(e *poolEntry, now time.Time) bool {
	if !e.idleFrom.IsZero() && now.Sub(e.idleFrom) > p.idleTTL {
		return true
	}
	return now.Sub(e.opened) > p.maxAge
}

// trimIdleLocked evicts idle entries while the pool holds more than maxIdle of
// them, oldest first. Only idle sessions count toward the bound: an in-use
// session is not "kept open for later", it is in service now. Called with
// p.mu held; the returned entries must be torn down after the lock is dropped.
func (p *sessionPool) trimIdleLocked() []*poolEntry {
	idle := 0
	for _, e := range p.entries {
		if e.refs == 0 {
			idle++
		}
	}
	var victims []*poolEntry
	for idle > p.maxIdle {
		var (
			victimKey poolKey
			victim    *poolEntry
		)
		for key, e := range p.entries {
			if e.refs > 0 {
				continue
			}
			if victim == nil || e.idleFrom.Before(victim.idleFrom) {
				victimKey, victim = key, e
			}
		}
		if victim == nil {
			break // every entry is in use; nothing idle to evict
		}
		delete(p.entries, victimKey)
		victims = append(victims, victim)
		idle--
	}
	return victims
}

// shutdown tears the pool down: every idle session is closed now, and in-use
// sessions are torn down when their lease is released (release checks closed).
func (p *sessionPool) shutdown() {
	p.mu.Lock()
	p.closed = true
	var doomed []*poolEntry
	for key, e := range p.entries {
		if e.refs > 0 {
			continue // released later; release sees closed
		}
		doomed = append(doomed, e)
		delete(p.entries, key)
	}
	p.mu.Unlock()
	for _, e := range doomed {
		e.sess.teardown()
	}
}

// len reports the number of pooled entries (idle + in use). Diagnostics only.
func (p *sessionPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// credentialFingerprint reduces the expanded credential to a stable, host-side
// key component. Nothing here is ever sent to the plugin; the fingerprint only
// answers "would these two leases have authenticated the same way?".
func credentialFingerprint(cred channel.CredentialRef) string {
	h := sha256.New()
	for _, part := range []string{cred.Username, cred.Password, cred.KeyPath, cred.KeyPassphrase} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
