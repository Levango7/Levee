// pool_test.go pins the cross-run pool's rules without spawning processes: the
// session is a fake, so every rule (reuse, refcount, TTL, age, bound, shutdown)
// is asserted directly instead of being inferred from process behaviour.
package plugin

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePoolSession is a poolSession with an alive flag and a teardown counter.
type fakePoolSession struct {
	mu    sync.Mutex
	alive bool
	tears int
	reqs  int
}

func newFakePoolSession() *fakePoolSession { return &fakePoolSession{alive: true} }

func (f *fakePoolSession) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}

func (f *fakePoolSession) Request(context.Context, HostRequest) (PluginResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs++
	return PluginResponse{OK: true}, nil
}

func (f *fakePoolSession) teardown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tears++
	f.alive = false
}

func (f *fakePoolSession) teardowns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tears
}

// kill simulates the plugin dying under the pool's feet.
func (f *fakePoolSession) kill() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = false
}

func testKey(host string) poolKey {
	return poolKey{plugin: "acme", host: host, port: 22, typ: "acme-rest", credFP: "fp"}
}

// clock lets the age/TTL rules be tested without sleeping.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestPool() (*sessionPool, *clock) {
	p := newSessionPool()
	c := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	p.now = c.now
	return p, c
}

// A session published and released is handed to the next acquire — the whole
// point of the pool.
func TestPoolReusesIdleSession(t *testing.T) {
	p, _ := newTestPool()
	sess := newFakePoolSession()
	key := testKey("h1")

	got, ok := p.publish(key, sess)
	require.True(t, ok)
	require.Same(t, sess, got)

	p.release(key, sess)
	require.Equal(t, 1, p.len(), "a released session stays pooled")

	again, ok := p.acquire(key)
	require.True(t, ok)
	assert.Same(t, sess, again, "the next run must get the same session")
	assert.Equal(t, 0, sess.teardowns())
}

// Two leases on one session do not outlive each other: the session goes idle
// only when the last one is released.
func TestPoolRefCountKeepsSessionInUse(t *testing.T) {
	p, _ := newTestPool()
	sess := newFakePoolSession()
	key := testKey("h1")
	p.publish(key, sess)

	second, ok := p.acquire(key)
	require.True(t, ok)
	require.Same(t, sess, second)

	p.release(key, sess)
	assert.Equal(t, 1, p.len(), "one lease is still out")

	p.sweep()
	assert.Equal(t, 0, sess.teardowns(), "an in-use session is never reaped")

	p.release(key, sess)
	p.sweep()
	assert.Equal(t, 0, sess.teardowns(), "idle but not past TTL is kept")
}

// A dead session is dropped on sight, never handed to the next run.
func TestPoolDropsDeadSession(t *testing.T) {
	p, _ := newTestPool()
	sess := newFakePoolSession()
	key := testKey("h1")
	p.publish(key, sess)
	p.release(key, sess)

	sess.kill()

	_, ok := p.acquire(key)
	assert.False(t, ok, "a dead session must not be handed out")
	assert.Equal(t, 0, p.len())
	assert.Equal(t, 1, sess.teardowns())
}

// Idle past the TTL is reaped by the sweeper.
func TestPoolReapsIdlePastTTL(t *testing.T) {
	p, clk := newTestPool()
	sess := newFakePoolSession()
	key := testKey("h1")
	p.publish(key, sess)
	p.release(key, sess)

	clk.advance(idleSessionTTL + time.Second)
	p.sweep()

	assert.Equal(t, 0, p.len())
	assert.Equal(t, 1, sess.teardowns())
}

// Age is the second bound: a session that never goes idle still expires once
// it is released, even if it was used a moment ago.
func TestPoolReapsByAgeNotJustIdle(t *testing.T) {
	p, clk := newTestPool()
	p.maxAge = time.Minute
	sess := newFakePoolSession()
	key := testKey("h1")
	p.publish(key, sess) // opened at T0

	clk.advance(59 * time.Second)
	p.release(key, sess) // idle since T+59s
	p.sweep()
	assert.Equal(t, 0, sess.teardowns(), "not old enough yet")

	clk.advance(2 * time.Second) // now past maxAge, still inside idleTTL
	p.sweep()
	assert.Equal(t, 0, p.len())
	assert.Equal(t, 1, sess.teardowns())
}

// An in-use session past maxAge is kept: pulling it out would fail the run
// holding it. It is reaped at the next release.
func TestPoolNeverReapsInUseSessionByAge(t *testing.T) {
	p, clk := newTestPool()
	p.maxAge = time.Minute
	sess := newFakePoolSession()
	key := testKey("h1")
	p.publish(key, sess) // refs = 1

	clk.advance(2 * p.maxAge)
	p.sweep()
	assert.Equal(t, 0, sess.teardowns())
	require.True(t, sess.Alive())

	p.release(key, sess)
	p.sweep()
	assert.Equal(t, 1, sess.teardowns(), "released past its age, it goes")
}

// The idle bound evicts the oldest idle entry, never an in-use one.
func TestPoolEvictsOldestIdleOverBound(t *testing.T) {
	p, clk := newTestPool()
	p.maxIdle = 2

	oldest := newFakePoolSession()
	keyOld := testKey("old")
	p.publish(keyOld, oldest)
	p.release(keyOld, oldest) // idle #1

	clk.advance(time.Second)
	middle := newFakePoolSession()
	keyMid := testKey("mid")
	p.publish(keyMid, middle)
	p.release(keyMid, middle) // idle #2

	clk.advance(time.Second)
	busy := newFakePoolSession()
	keyBusy := testKey("busy")
	p.publish(keyBusy, busy) // in use; does not count toward the idle bound
	assert.Equal(t, 0, oldest.teardowns(), "an in-use session is not an idle session")

	clk.advance(time.Second)
	newest := newFakePoolSession()
	keyNew := testKey("new")
	p.publish(keyNew, newest)
	p.release(keyNew, newest) // idle #3 → evict the oldest

	assert.Equal(t, 1, oldest.teardowns(), "the oldest idle entry is the eviction victim")
	assert.Equal(t, 0, middle.teardowns())
	assert.Equal(t, 0, busy.teardowns(), "an in-use session is never evicted")
	assert.Equal(t, 3, p.len())
}

// publish resolves the dial race: exactly one session survives per key and the
// loser is torn down instead of leaking.
func TestPoolPublishRaceKeepsOneSessionPerKey(t *testing.T) {
	p, _ := newTestPool()
	key := testKey("h1")

	winner := newFakePoolSession()
	got, ok := p.publish(key, winner)
	require.True(t, ok)
	require.Same(t, winner, got)

	loser := newFakePoolSession()
	got, ok = p.publish(key, loser)
	require.True(t, ok)
	assert.Same(t, winner, got, "the second dialer is handed the published session")
	assert.Equal(t, 1, loser.teardowns(), "the loser's process must not leak")
	assert.Equal(t, 1, p.len())
}

// Shutdown tears down idle sessions immediately, in-use ones at release, and
// refuses anything new.
func TestPoolShutdown(t *testing.T) {
	p, _ := newTestPool()
	idle := newFakePoolSession()
	keyIdle := testKey("idle")
	p.publish(keyIdle, idle)
	p.release(keyIdle, idle)

	busy := newFakePoolSession()
	keyBusy := testKey("busy")
	p.publish(keyBusy, busy) // still in use

	p.shutdown()

	assert.Equal(t, 1, idle.teardowns(), "idle sessions go at shutdown")
	assert.Equal(t, 0, busy.teardowns(), "an in-use session is not yanked from under its run")
	assert.Equal(t, 1, p.len())

	p.release(keyBusy, busy)
	assert.Equal(t, 1, busy.teardowns(), "released after shutdown, it goes")

	_, ok := p.acquire(keyBusy)
	assert.False(t, ok)

	late := newFakePoolSession()
	_, ok = p.publish(testKey("late"), late)
	assert.False(t, ok)
	assert.Equal(t, 1, late.teardowns(), "a dial that finishes after shutdown is torn down")
}

// The fingerprint is the credential boundary: different secrets must not share
// a session, and the same secret must produce the same key.
func TestCredentialFingerprintSeparatesSecrets(t *testing.T) {
	base := channel.CredentialRef{Username: "ops", Password: "s3cret"}
	assert.Equal(t, credentialFingerprint(base), credentialFingerprint(base))

	cases := []channel.CredentialRef{
		{Username: "ops", Password: "other"},
		{Username: "other", Password: "s3cret"},
		{Username: "ops", Password: "s3cret", KeyPath: "/k"},
		{Username: "ops", Password: "s3cret", KeyPassphrase: "p"},
	}
	for _, other := range cases {
		assert.NotEqual(t, credentialFingerprint(base), credentialFingerprint(other),
			"a different credential must produce a different pool key: %+v", other)
	}
}
