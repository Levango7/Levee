package verify

// Per-target execution for remote probes. The engine's run path supplies a
// channel provider rather than a single channel (see gate.go), so a probe that
// declares `mode: remote` must measure every machine in the batch from that
// machine — and a batch may never be reported healthy because one host said so.
//
// These cases pin the four rules that make the verdict honest, plus the two
// shapes that must NOT change: the legacy single-channel call and a direct
// probe (which measures from the controller and must never fan out).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

// scriptedProvider hands each host its own channel and records which hosts were
// dialed, in order.
type scriptedProvider struct {
	channels map[string]*fakeChannel
	// overrides carries stubs that are not *fakeChannel (e.g. one that cancels
	// the caller's context mid-run). It wins over channels for that host.
	overrides map[string]channel.Channel
	dialErr   map[string]error
	dialed    []string
}

func (p *scriptedProvider) get(_ context.Context, host string) (channel.Channel, error) {
	p.dialed = append(p.dialed, host)
	if err, bad := p.dialErr[host]; bad {
		return nil, err
	}
	if ch, ok := p.overrides[host]; ok {
		return ch, nil
	}
	ch, ok := p.channels[host]
	if !ok {
		return nil, errors.New("no channel scripted for " + host)
	}
	return ch, nil
}

func newHTTPRemoteGate(name string) *ProbeGate {
	return NewProbeGate(name, PhasePostBatch, map[string]any{
		"kind": "http",
		"mode": "remote",
		"url":  "http://service.local/health",
	})
}

func TestProbeRemoteAllTargetsMustPass(t *testing.T) {
	provider := &scriptedProvider{channels: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "200")),
		"web-2": newFakeChannel(execResult(0, "204")),
	}}
	g := newHTTPRemoteGate("http-remote-ok")

	res, err := g.Check(context.Background(), GateInput{
		RunID:      "run-1",
		TargetIDs:  []string{"web-1", "web-2"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.ElementsMatch(t, []string{"web-1", "web-2"}, provider.dialed,
		"each declared target must be probed from its own machine")
	perTarget, ok := res.Details["targets"].(map[string]any)
	require.True(t, ok, "details must carry per-target evidence: %#v", res.Details["targets"])
	require.Len(t, perTarget, 2)
	for _, host := range []string{"web-1", "web-2"} {
		entry, ok := perTarget[host].(map[string]any)
		require.True(t, ok, "missing entry for %s", host)
		assert.Equal(t, true, entry["passed"], "target %s: %#v", host, entry)
	}
}

func TestProbeRemoteOneFailingTargetBlocksTheBatch(t *testing.T) {
	provider := &scriptedProvider{channels: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "200")),
		"web-2": newFakeChannel(execResult(0, "503")),
	}}
	g := newHTTPRemoteGate("http-remote-one-bad")

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs:  []string{"web-1", "web-2"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "a healthy host must not carry the batch")
	assert.Contains(t, res.Message, "web-2")
	assert.Equal(t, []string{"web-2"}, res.Details["failed_targets"],
		"the offender must be named, not just counted")

	// The healthy target must still be recorded, or the audit trail reads as
	// though web-1 had never been checked.
	perTarget := res.Details["targets"].(map[string]any)
	assert.Equal(t, true, perTarget["web-1"].(map[string]any)["passed"])
	assert.Equal(t, false, perTarget["web-2"].(map[string]any)["passed"])
}

func TestProbeRemoteUnreachableTargetCountsAsFailure(t *testing.T) {
	provider := &scriptedProvider{
		channels: map[string]*fakeChannel{"web-1": newFakeChannel(execResult(0, "200"))},
		dialErr:  map[string]error{"web-2": errors.New("ssh: connection refused")},
	}
	g := newHTTPRemoteGate("http-remote-dial-fail")

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs:  []string{"web-1", "web-2"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "an undialable host has not been shown healthy")
	assert.Contains(t, res.Message, "web-2")
	assert.Contains(t, provider.dialed, "web-2", "the dial attempt itself must be recorded")
	assert.Len(t, provider.dialed, 2)

	perTarget := res.Details["targets"].(map[string]any)
	entry := perTarget["web-2"].(map[string]any)
	assert.Equal(t, false, entry["passed"])
	assert.Contains(t, entry["error"], "connection refused")
}

func TestProbeRemoteWithoutTargetsFailsClosed(t *testing.T) {
	calls := 0
	provider := func(context.Context, string) (channel.Channel, error) {
		calls++
		return newFakeChannel(execResult(0, "200")), nil
	}
	g := newHTTPRemoteGate("http-remote-no-targets")

	res, err := g.Check(context.Background(), GateInput{ChannelFor: provider})
	require.NoError(t, err)
	assert.False(t, res.Passed)
	assert.Equal(t, "no_targets", res.Details["reason"])
	assert.Zero(t, calls, "nothing may be dialed when the phase names no targets")
}

func TestProbeRemoteTCPPortFromTargetProbesEachHostOwnAddress(t *testing.T) {
	provider := &scriptedProvider{channels: map[string]*fakeChannel{
		"10.0.0.1:6379": newFakeChannel(execResult(0, "")),
		"10.0.0.2:6379": newFakeChannel(execResult(0, "")),
	}}
	g := NewProbeGate("tcp-remote-per-target", PhasePostBatch, map[string]any{
		"kind":             "tcp",
		"mode":             "remote",
		"port_from_target": true,
	})

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs:  []string{"10.0.0.1:6379", "10.0.0.2:6379"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)

	// Narrowing to one target per iteration is what stops host A's channel from
	// reporting on host B's port: each channel must see exactly its own address.
	for host, ch := range provider.channels {
		cmds := ch.commandsCopy()
		require.Len(t, cmds, 1, "%s should have been probed once", host)
		assert.Contains(t, cmds[0], "/dev/tcp/"+host)
		for other := range provider.channels {
			if other != host {
				assert.NotContains(t, cmds[0], other)
			}
		}
	}
}

func TestProbeScriptKindRunsOnEveryTarget(t *testing.T) {
	provider := &scriptedProvider{channels: map[string]*fakeChannel{
		"web-1": newFakeChannel(execResult(0, "")),
		"web-2": newFakeChannel(execResult(1, "")),
	}}
	g := NewProbeGate("script-per-target", PhasePostBatch, map[string]any{
		"kind":   "script",
		"script": "test -f /run/flag",
	})

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs:  []string{"web-1", "web-2"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "script kind consumes the provider too")
	assert.Contains(t, res.Message, "web-2")
	assert.Equal(t, []string{"web-2"}, res.Details["failed_targets"])
}

func TestProbeDirectModeNeverDialsTargets(t *testing.T) {
	// A direct probe measures reachability from the controller. Wiring it to
	// the provider would dial every target for nothing and turn a cheap check
	// into an N x fan-out, so the provider must stay untouched.
	calls := 0
	provider := func(context.Context, string) (channel.Channel, error) {
		calls++
		return newFakeChannel(execResult(0, "200")), nil
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := NewProbeGate("http-direct-no-fanout", PhasePostBatch, map[string]any{
		"kind": "http",
		"url":  srv.URL,
	})

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs:  []string{"web-1", "web-2", "web-3"},
		ChannelFor: provider,
	})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.Zero(t, calls, "direct probes must not open target channels")
}

func TestProbeRemoteLegacySingleChannelPathUnchanged(t *testing.T) {
	// Callers that already hold one channel (the CLI's `levee verify`, and any
	// future non-run path) must keep the pre-provider behaviour exactly.
	ch := newFakeChannel(execResult(0, "200"))
	g := newHTTPRemoteGate("http-remote-legacy")

	res, err := g.Check(context.Background(), GateInput{
		RunID:   "run-9",
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, res.Passed, "message: %s", res.Message)
	assert.Equal(t, int64(1), ch.calls.Load())
}

func TestProbeRemoteWithoutProviderStillFailsClosed(t *testing.T) {
	g := newHTTPRemoteGate("http-remote-no-provider")

	res, err := g.Check(context.Background(), GateInput{
		TargetIDs: []string{"web-1"},
	})
	require.Error(t, err)
	assert.False(t, res.Passed)
	assert.Contains(t, err.Error(), "channel is nil")
	assert.Equal(t, "missing_channel", res.Details["reason"])
}

// cancelAfterFirstExec cancels the caller's context once the first probe has
// returned, so the loop's per-target cancellation check is exercised
// deterministically instead of by racing a timer against a sleep.
type cancelAfterFirstExec struct {
	*fakeChannel
	cancel context.CancelFunc
}

func (c *cancelAfterFirstExec) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	res, err := c.fakeChannel.Exec(ctx, cmd)
	c.cancel()
	return res, err
}

func TestProbeRemoteContextCancelledStopsBeforeNextTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancelling inside the first probe's Exec means the loop's per-target
	// check is what reacts, not the up-front guard in Check.
	first := &cancelAfterFirstExec{
		fakeChannel: newFakeChannel(execResult(0, "200")),
		cancel:      cancel,
	}
	second := newFakeChannel(execResult(0, "200"))
	provider := &scriptedProvider{
		channels:  map[string]*fakeChannel{"web-2": second},
		overrides: map[string]channel.Channel{"web-1": first},
	}
	g := newHTTPRemoteGate("http-remote-cancel")

	res, err := g.Check(ctx, GateInput{
		TargetIDs:  []string{"web-1", "web-2"},
		ChannelFor: provider.get,
	})
	require.NoError(t, err)
	assert.False(t, res.Passed, "an unfinished check is never a pass")
	assert.Equal(t, "context_cancelled", res.Details["reason"])
	assert.NotContains(t, provider.dialed, "web-2",
		"the loop must stop before claiming evidence it does not have")
	assert.Zero(t, second.calls.Load())
}
