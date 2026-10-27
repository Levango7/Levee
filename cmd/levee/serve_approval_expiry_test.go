package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/config"
)

// TestApprovalExpiryResolverMapsTiers pins the tier→timeout mapping the
// kickoff records carry: the three documented timeouts (standard 4h / high
// 8h / emergency 24h) become the record's ExpiresAt through this resolver,
// and the expiry sweeper enforces them. Before this resolver the kickoff
// hardcoded 24h for every tier, so the config was decoration.
func TestApprovalExpiryResolverMapsTiers(t *testing.T) {
	cfg := &config.Config{}
	cfg.Approval.StandardTimeout = 4 * time.Hour
	cfg.Approval.HighTimeout = 8 * time.Hour
	cfg.Approval.EmergencySupplement = 24 * time.Hour

	resolve := approvalExpiryResolverFromConfig(cfg)
	now := time.Now().UTC()

	std := resolve(approval.LevelStandard)
	require.False(t, std.IsZero())
	assert.WithinDuration(t, now.Add(4*time.Hour), std, 5*time.Second)

	high := resolve(approval.LevelHigh)
	require.False(t, high.IsZero())
	assert.WithinDuration(t, now.Add(8*time.Hour), high, 5*time.Second)

	emerg := resolve(approval.LevelEmergency)
	require.False(t, emerg.IsZero())
	assert.WithinDuration(t, now.Add(24*time.Hour), emerg, 5*time.Second)
}

// TestApprovalExpiryResolverZeroMeansNoTimeout pins the opt-out: a tier with
// a non-positive timeout (and unknown levels) yields the zero time, which
// approval.CheckExpiry treats as "no timeout configured" — the documented
// skip, not a silent 24h.
func TestApprovalExpiryResolverZeroMeansNoTimeout(t *testing.T) {
	cfg := &config.Config{}
	cfg.Approval.StandardTimeout = 4 * time.Hour
	// High timeout deliberately left zero.

	resolve := approvalExpiryResolverFromConfig(cfg)

	assert.True(t, resolve(approval.LevelHigh).IsZero(),
		"a zero timeout tier must opt out of expiry, not fall back to a default")
	assert.True(t, resolve("not-a-level").IsZero(),
		"an unknown level must not guess a timeout")
}

// TestExpirySweeperNotifiesBothMirrors pins the wiring the two mirrors need put
// back in step: the expiry callback must tell BOTH the ChatOps bridge and the
// Jira bridge, or the un-notified one keeps showing an approval as open after
// it timed out. runServe is a long boot sequence a unit test cannot drive, so
// (like the other serve wiring guards) this reads cmd_serve.go and asserts both
// calls sit inside the sweeper's announcement callback.
func TestExpirySweeperNotifiesBothMirrors(t *testing.T) {
	src, err := os.ReadFile("cmd_serve.go")
	require.NoError(t, err)
	text := string(src)

	// Anchor on the sweeper's callback signature; if that moves, this guard
	// must be re-pointed rather than pass vacuously.
	cb := strings.Index(text, "func(ctx context.Context, expired []approval.Approval) {")
	require.Greater(t, cb, -1,
		"the expiry sweeper callback anchor moved; this wiring guard can no longer see it")
	body := text[cb:]

	require.Greater(t, strings.Index(body, "chatOpsBridge.OnApprovalExpired(&a)"), -1,
		"the expiry sweeper must announce to the ChatOps mirror")
	require.Greater(t, strings.Index(body, "jiraBridge.OnApprovalExpired(&a)"), -1,
		"the expiry sweeper must announce to the Jira mirror (parity with the room)")
}

// TestApprovalExpirySweeperIntervalDefault pins the config default: the
// sweep runs every minute unless a deployment opts out with <= 0. Without
// the default the sweeper would silently disable itself on every deployment
// that did not spell the key out.
func TestApprovalExpirySweeperIntervalDefault(t *testing.T) {
	loaded, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, time.Minute, loaded.Approval.ExpirySweepInterval)
}
