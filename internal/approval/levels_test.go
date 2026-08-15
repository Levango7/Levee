package approval

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =========================================================================
// Level constants
// =========================================================================

func TestLevelConstants(t *testing.T) {
	// The three level constants must match the strings used in
	// service.go's validLevel switch and in the LEVEELang spec.
	assert.Equal(t, "standard", LevelStandard)
	assert.Equal(t, "high", LevelHigh)
	assert.Equal(t, "emergency", LevelEmergency)
}

func TestLevelConstants_AreValidLevels(t *testing.T) {
	// Each level constant must be accepted by validLevel (defined in
	// service.go). This guards against a rename in one place but not
	// the other.
	for _, level := range []string{LevelStandard, LevelHigh, LevelEmergency} {
		assert.True(t, validLevel(level), "level %q should be valid", level)
	}
}

// =========================================================================
// NewLevelManager — default configuration
// =========================================================================

func TestNewLevelManager_StandardDefaults(t *testing.T) {
	m := NewLevelManager()
	cfg, err := m.Get(LevelStandard)
	require.NoError(t, err)
	assert.Equal(t, LevelStandard, cfg.Level)
	assert.Equal(t, 1, cfg.RequiredApprovers)
	assert.Equal(t, 1, cfg.MinApprovers)
	assert.Equal(t, 24*time.Hour, cfg.Timeout)
	assert.Equal(t, EscalateNotify, cfg.EscalationPolicy.OnTimeout)
}

func TestNewLevelManager_HighDefaults(t *testing.T) {
	m := NewLevelManager()
	cfg, err := m.Get(LevelHigh)
	require.NoError(t, err)
	assert.Equal(t, LevelHigh, cfg.Level)
	assert.Equal(t, 2, cfg.RequiredApprovers)
	assert.Equal(t, 2, cfg.MinApprovers)
	assert.Equal(t, 4*time.Hour, cfg.Timeout)
	assert.Equal(t, EscalateEscalate, cfg.EscalationPolicy.OnTimeout)
	assert.Equal(t, LevelEmergency, cfg.EscalationPolicy.EscalateTo)
}

func TestNewLevelManager_EmergencyDefaults(t *testing.T) {
	m := NewLevelManager()
	cfg, err := m.Get(LevelEmergency)
	require.NoError(t, err)
	assert.Equal(t, LevelEmergency, cfg.Level)
	assert.Equal(t, 1, cfg.RequiredApprovers)
	assert.Equal(t, 1, cfg.MinApprovers)
	assert.Equal(t, 30*time.Minute, cfg.Timeout)
	assert.Equal(t, EscalateAutoReject, cfg.EscalationPolicy.OnTimeout)
}

func TestNewLevelManager_AllReturnsThreeTiersInOrder(t *testing.T) {
	m := NewLevelManager()
	all := m.All()
	require.Len(t, all, 3)
	assert.Equal(t, LevelStandard, all[0].Level)
	assert.Equal(t, LevelHigh, all[1].Level)
	assert.Equal(t, LevelEmergency, all[2].Level)
}

// =========================================================================
// Get
// =========================================================================

func TestGet_InvalidLevel(t *testing.T) {
	m := NewLevelManager()
	_, err := m.Get("super-high")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidLevel)
	assert.Contains(t, err.Error(), "super-high")
}

func TestGet_EmptyLevel(t *testing.T) {
	m := NewLevelManager()
	_, err := m.Get("")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidLevel)
}

func TestGet_AllThreeLevelsSucceed(t *testing.T) {
	m := NewLevelManager()
	for _, level := range []string{LevelStandard, LevelHigh, LevelEmergency} {
		cfg, err := m.Get(level)
		require.NoError(t, err, "level %q should be retrievable", level)
		assert.Equal(t, level, cfg.Level)
	}
}

// =========================================================================
// SetConfig
// =========================================================================

func TestSetConfig_OverridesDefaults(t *testing.T) {
	m := NewLevelManager()

	// Tighten the high-tier timeout to 1h in a regulated environment.
	newHigh := LevelConfig{
		Level:             LevelHigh,
		TriggerCondition:  "regulated: irreversible ops, 3 approvers, 1h timeout",
		RequiredApprovers: 3,
		MinApprovers:      3,
		Timeout:           1 * time.Hour,
		EscalationPolicy:  EscalationPolicy{OnTimeout: EscalateAutoReject},
	}
	require.NoError(t, m.SetConfig(newHigh))

	got, err := m.Get(LevelHigh)
	require.NoError(t, err)
	assert.Equal(t, 3, got.RequiredApprovers)
	assert.Equal(t, 3, got.MinApprovers)
	assert.Equal(t, 1*time.Hour, got.Timeout)
	assert.Equal(t, EscalateAutoReject, got.EscalationPolicy.OnTimeout)
	assert.Contains(t, got.TriggerCondition, "regulated")
}

func TestSetConfig_InvalidLevel(t *testing.T) {
	m := NewLevelManager()
	err := m.SetConfig(LevelConfig{Level: "super-high"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidLevel)
}

func TestSetConfig_DoesNotAffectOtherTiers(t *testing.T) {
	m := NewLevelManager()
	original, err := m.Get(LevelStandard)
	require.NoError(t, err)

	require.NoError(t, m.SetConfig(LevelConfig{
		Level:             LevelHigh,
		RequiredApprovers: 5,
		MinApprovers:      5,
		Timeout:           1 * time.Minute,
	}))

	standard, err := m.Get(LevelStandard)
	require.NoError(t, err)
	assert.Equal(t, original, standard, "standard tier should be unchanged")
}

// =========================================================================
// DetermineLevel — priority order
// =========================================================================

func TestDetermineLevel_ReversibleStepGoesStandard(t *testing.T) {
	m := NewLevelManager()
	step := Step{Module: "pkg", Action: "install"}
	cfg := m.DetermineLevel(step)
	assert.Equal(t, LevelStandard, cfg.Level)
	assert.Equal(t, LevelStandard, m.DetermineLevelName(step))
}

func TestDetermineLevel_IrreversibleStepGoesHigh(t *testing.T) {
	m := NewLevelManager()
	step := Step{Module: "pkg", Action: "remove", Irreversible: true}
	cfg := m.DetermineLevel(step)
	assert.Equal(t, LevelHigh, cfg.Level)
	assert.Equal(t, 2, cfg.MinApprovers, "high tier should require 2 approvers")
}

func TestDetermineLevel_EmergencyStepGoesEmergency(t *testing.T) {
	m := NewLevelManager()
	step := Step{Module: "shell", Action: "exec", Emergency: true}
	cfg := m.DetermineLevel(step)
	assert.Equal(t, LevelEmergency, cfg.Level)
	assert.Equal(t, 30*time.Minute, cfg.Timeout)
}

func TestDetermineLevel_EmergencyBeatsIrreversible(t *testing.T) {
	// When both Emergency and Irreversible are true, Emergency wins.
	// This is the break-glass semantics: the operator has explicitly
	// opted into the fast track.
	m := NewLevelManager()
	step := Step{
		Module:       "pkg",
		Action:       "remove",
		Irreversible: true,
		Emergency:    true,
	}
	cfg := m.DetermineLevel(step)
	assert.Equal(t, LevelEmergency, cfg.Level,
		"emergency should take priority over irreversible")
}

func TestDetermineLevel_ZeroStepGoesStandard(t *testing.T) {
	// A zero-value Step is neither irreversible nor emergency, so it
	// lands in the standard tier.
	m := NewLevelManager()
	cfg := m.DetermineLevel(Step{})
	assert.Equal(t, LevelStandard, cfg.Level)
}

func TestDetermineLevel_ReturnsFullConfig(t *testing.T) {
	// DetermineLevel returns the full LevelConfig, not just the name,
	// so callers can read MinApprovers / Timeout without a second
	// lookup.
	m := NewLevelManager()
	cfg := m.DetermineLevel(Step{Irreversible: true})
	assert.Equal(t, LevelHigh, cfg.Level)
	assert.Equal(t, 2, cfg.MinApprovers)
	assert.Equal(t, 4*time.Hour, cfg.Timeout)
	assert.Equal(t, EscalateEscalate, cfg.EscalationPolicy.OnTimeout)
}

func TestDetermineLevel_RespectsSetConfig(t *testing.T) {
	// When a tier is overridden via SetConfig, DetermineLevel must
	// return the overridden config, not the default.
	m := NewLevelManager()
	override := LevelConfig{
		Level:             LevelHigh,
		RequiredApprovers: 5,
		MinApprovers:      5,
		Timeout:           90 * time.Minute,
		EscalationPolicy:  EscalationPolicy{OnTimeout: EscalateAutoReject},
	}
	require.NoError(t, m.SetConfig(override))

	cfg := m.DetermineLevel(Step{Irreversible: true})
	assert.Equal(t, 5, cfg.MinApprovers)
	assert.Equal(t, 90*time.Minute, cfg.Timeout)
}

// =========================================================================
// DetermineLevelName
// =========================================================================

func TestDetermineLevelName_MatchesDetermineLevel(t *testing.T) {
	m := NewLevelManager()
	for _, step := range []Step{
		{Module: "pkg", Action: "install"},
		{Module: "pkg", Action: "remove", Irreversible: true},
		{Module: "shell", Action: "exec", Emergency: true},
		{Module: "pkg", Action: "remove", Irreversible: true, Emergency: true},
	} {
		cfg := m.DetermineLevel(step)
		name := m.DetermineLevelName(step)
		assert.Equal(t, cfg.Level, name)
	}
}

// =========================================================================
// Step struct
// =========================================================================

func TestStep_ZeroValue(t *testing.T) {
	var s Step
	assert.Empty(t, s.Module)
	assert.Empty(t, s.Action)
	assert.Empty(t, s.Target)
	assert.False(t, s.Irreversible)
	assert.False(t, s.Emergency)
}

// =========================================================================
// EscalationPolicy
// =========================================================================

func TestEscalationPolicy_ZeroValue(t *testing.T) {
	var p EscalationPolicy
	assert.Empty(t, p.OnTimeout)
	assert.Empty(t, p.EscalateTo)
	assert.Empty(t, p.NotifyApprovers)
}

func TestEscalationActionConstants(t *testing.T) {
	// The three action constants must be distinct non-empty strings.
	actions := []string{EscalateNotify, EscalateEscalate, EscalateAutoReject}
	for _, a := range actions {
		assert.NotEmpty(t, a)
	}
	assert.NotEqual(t, EscalateNotify, EscalateEscalate)
	assert.NotEqual(t, EscalateNotify, EscalateAutoReject)
	assert.NotEqual(t, EscalateEscalate, EscalateAutoReject)
}

// =========================================================================
// Concurrency
// =========================================================================

func TestLevelManager_ConcurrentDetermineLevel(t *testing.T) {
	// DetermineLevel is read-only on the configs map and must be safe
	// for concurrent use. This test exercises the lock under -race.
	m := NewLevelManager()
	steps := []Step{
		{Module: "pkg", Action: "install"},
		{Module: "pkg", Action: "remove", Irreversible: true},
		{Module: "shell", Action: "exec", Emergency: true},
	}

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = m.DetermineLevel(steps[i%len(steps)])
			_, _ = m.Get(LevelHigh)
		}(i)
	}
	wg.Wait()
}

func TestLevelManager_ConcurrentSetAndGet(t *testing.T) {
	// SetConfig and Get can run concurrently; the test just exercises
	// the map under -race to detect data races.
	m := NewLevelManager()

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_ = m.SetConfig(LevelConfig{
				Level:             LevelHigh,
				RequiredApprovers: 2,
				MinApprovers:      2,
				Timeout:           4 * time.Hour,
			})
		}()
		go func() {
			defer wg.Done()
			_, _ = m.Get(LevelHigh)
			_ = m.All()
		}()
	}
	wg.Wait()
}

// =========================================================================
// Integration with service.go
// =========================================================================

func TestLevelConfig_CanBeUsedInCreateRequest(t *testing.T) {
	// The LevelConfig returned by DetermineLevel must be usable to
	// build a CreateRequest that the Service accepts. This is the
	// intended integration: the planner calls DetermineLevel, then
	// copies the level name and MinApprovers into CreateRequest.
	m := NewLevelManager()
	svc, _ := newService(t)

	step := Step{Module: "pkg", Action: "remove", Irreversible: true}
	cfg := m.DetermineLevel(step)

	a, err := svc.Create(bgCtx(), CreateRequest{
		RunID:        "run-1",
		Level:        cfg.Level,
		Approvers:    []string{"alice", "bob"},
		MinApprovers: cfg.MinApprovers,
		ExpiresAt:    futureTime(),
	})
	require.NoError(t, err)
	assert.Equal(t, LevelHigh, a.Level)
	assert.Equal(t, 2, a.MinApprovers)
}

func TestLevelConfig_InvalidLevelRejectedByService(t *testing.T) {
	// Sanity check: a level name that is not one of the three legal
	// tiers is rejected by the service. This documents the contract
	// between LevelManager and Service.
	svc, _ := newService(t)
	_, err := svc.Create(bgCtx(), CreateRequest{
		RunID: "run-1",
		Level: "super-high",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidLevel))
}
