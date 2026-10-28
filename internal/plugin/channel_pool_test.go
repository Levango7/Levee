// channel_pool_test.go pins the cross-run half of the channel session story
// (D-1c) against REAL processes: the pool hands a live plugin session from one
// run to the next instead of paying for the dial again, never shares one across
// targets or credentials, and stops when serve's lifecycle context ends.
//
// The child personality answers "pid" with its own process id, so "same
// session" and "fresh dial" are facts about processes, not about which Go
// handle came back: a pooled session is the same PID, a re-dial is not.
package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newChannelFixtureFactory builds a factory whose pool is torn down with the
// test — the same lifecycle serve gives it (its context), only synchronous so
// no child outlives the test binary.
func newChannelFixtureFactory(t *testing.T) *PluginChannelFactory {
	t.Helper()
	t.Setenv(childEnvKey, "channel")

	ctx, cancel := context.WithCancel(context.Background())
	f, err := NewPluginChannelFactory(ctx, channelRecord("acme-rest"))
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		f.pool.shutdown()
	})
	return f
}

func channelForTest(t *testing.T, f *PluginChannelFactory, target channel.Target) *PluginChannel {
	t.Helper()
	ch, err := f.Create(target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ch.Close() })
	require.NoError(t, ch.Connect(context.Background()))
	return ch.(*PluginChannel)
}

func execPID(t *testing.T, ch *PluginChannel) string {
	t.Helper()
	res, err := ch.Exec(context.Background(), "pid")
	require.NoError(t, err)
	require.NotEmpty(t, res.Stdout)
	return res.Stdout
}

// A run ends by Close; the next run with the same target and credential must
// land on the SAME plugin process — that reuse is the whole point of D-1c.
func TestPluginChannelPoolReusesSessionAcrossRuns(t *testing.T) {
	f := newChannelFixtureFactory(t)
	target := channelTestTarget{host: "h1", port: 22, typ: "acme-rest"}

	first := channelForTest(t, f, target)
	pid1 := execPID(t, first)
	require.NoError(t, first.Close())
	assert.Equal(t, 1, f.pool.len(), "Close returns the lease; the session stays pooled")
	assert.False(t, first.IsConnected())

	second := channelForTest(t, f, target)
	pid2 := execPID(t, second)
	assert.Equal(t, pid1, pid2, "the second run must reuse the pooled session, not dial again")
	assert.Equal(t, 1, f.pool.len())
}

// The credential fingerprint is part of the key: two inventory rows that
// resolve to different secrets are different subjects and must never share a
// session, even when host, port and type match exactly.
func TestPluginChannelPoolNeverSharesAcrossCredentials(t *testing.T) {
	f := newChannelFixtureFactory(t)
	one := channelTestTarget{
		host: "h1", port: 22, typ: "acme-rest",
		cred: channel.CredentialRef{Username: "svc", Password: "one"},
	}
	two := one
	two.cred = channel.CredentialRef{Username: "svc", Password: "two"}

	first := channelForTest(t, f, one)
	pid1 := execPID(t, first)
	require.NoError(t, first.Close())

	second := channelForTest(t, f, two)
	pid2 := execPID(t, second)
	assert.NotEqual(t, pid1, pid2, "a different secret means a different subject: never share the session")
	assert.Equal(t, 2, f.pool.len(), "both sessions stay pooled, each under its own key")
}

// A session that died is never handed out: the next Connect dials a new
// process instead of reading from a stale pipe.
func TestPluginChannelPoolDialsAgainAfterSessionDeath(t *testing.T) {
	f := newChannelFixtureFactory(t)
	target := channelTestTarget{host: "h1", port: 22, typ: "acme-rest"}

	first := channelForTest(t, f, target)
	pid1 := execPID(t, first)
	_, err := first.Exec(context.Background(), "die")
	require.Error(t, err, "the child exited mid-request")
	require.False(t, first.IsConnected())

	second := channelForTest(t, f, target)
	pid2 := execPID(t, second)
	assert.NotEqual(t, pid1, pid2, "a dead session is never reused; the pool must dial again")
}

// serve's lifecycle context owns the pool: when it ends, idle sessions are torn
// down and a dial that would have reused the pool fails loudly instead of
// handing out a session from a shut-down pool.
func TestPluginChannelPoolStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Setenv(childEnvKey, "channel")
	f, err := NewPluginChannelFactory(ctx, channelRecord("acme-rest"))
	require.NoError(t, err)
	t.Cleanup(func() { f.pool.shutdown() })

	target := channelTestTarget{host: "h1", port: 22, typ: "acme-rest"}
	ch := channelForTest(t, f, target)
	require.NotEmpty(t, execPID(t, ch))

	sess := ch.session()
	require.NoError(t, ch.Close())
	require.True(t, sess.Alive(), "an idle session stays alive while pooled")

	cancel()

	require.Eventually(t, func() bool { return f.pool.len() == 0 }, 5*time.Second, 10*time.Millisecond,
		"shutdown must drop every idle session")
	require.Eventually(t, func() bool { return !sess.Alive() }, 5*time.Second, 10*time.Millisecond,
		"the pooled session must be torn down, not just forgotten")

	late, err := f.Create(target)
	require.NoError(t, err)
	err = late.Connect(context.Background())
	require.Error(t, err, "a shut-down pool must not hand out sessions")
	assert.Contains(t, err.Error(), "shutting down")
	_ = late.Close()
}

// Shutdown does not merely forget the entries: the child is waited on and its
// process state is set. Called synchronously here so the reap has finished
// before the assertion — the ctx-driven path above covers the wiring.
func TestPluginChannelPoolShutdownReapsTheSession(t *testing.T) {
	t.Setenv(childEnvKey, "channel")
	f, err := NewPluginChannelFactory(context.Background(), channelRecord("acme-rest"))
	require.NoError(t, err)

	target := channelTestTarget{host: "h1", port: 22, typ: "acme-rest"}
	ch := channelForTest(t, f, target)
	require.NotEmpty(t, execPID(t, ch))

	sess := ch.session().(*pluginSession)
	require.NoError(t, ch.Close())

	f.pool.shutdown()

	assert.False(t, sess.Alive())
	require.NotNil(t, sess.proc.cmd.ProcessState, "teardown must reap the child, not leak it")
}
