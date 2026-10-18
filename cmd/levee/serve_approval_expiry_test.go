package main

import (
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

// TestApprovalExpirySweeperIntervalDefault pins the config default: the
// sweep runs every minute unless a deployment opts out with <= 0. Without
// the default the sweeper would silently disable itself on every deployment
// that did not spell the key out.
func TestApprovalExpirySweeperIntervalDefault(t *testing.T) {
	loaded, err := config.Load("")
	require.NoError(t, err)
	assert.Equal(t, time.Minute, loaded.Approval.ExpirySweepInterval)
}
