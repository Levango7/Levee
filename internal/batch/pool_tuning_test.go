package batch

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- DefaultPoolTuningConfig ------------------------------------------------

func TestDefaultPoolTuningConfigValues(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	assert.Equal(t, 5, cfg.MaxConnectionsPerTarget, "default MaxConnectionsPerTarget")
	assert.Equal(t, 20, cfg.MaxConcurrentTargets, "default MaxConcurrentTargets")
	assert.Equal(t, 30*time.Second, cfg.ConnectionTimeout, "default ConnectionTimeout")
	assert.Equal(t, 5*time.Minute, cfg.IdleTimeout, "default IdleTimeout")
	assert.Equal(t, 10, cfg.BatchConcurrency, "default BatchConcurrency")
	assert.Equal(t, 100, cfg.TraceBatchSize, "default TraceBatchSize")
}

func TestDefaultPoolTuningConfigIsValid(t *testing.T) {
	cfg := DefaultPoolTuningConfig()
	assert.NoError(t, cfg.Validate(), "default config must pass validation")
}

// --- OptimizedPoolTuningConfig ----------------------------------------------

func TestOptimizedPoolTuningConfigValues(t *testing.T) {
	cfg := OptimizedPoolTuningConfig()

	assert.Equal(t, 10, cfg.MaxConnectionsPerTarget, "optimized MaxConnectionsPerTarget")
	assert.Equal(t, 100, cfg.MaxConcurrentTargets, "optimized MaxConcurrentTargets")
	assert.Equal(t, 15*time.Second, cfg.ConnectionTimeout, "optimized ConnectionTimeout")
	assert.Equal(t, 10*time.Minute, cfg.IdleTimeout, "optimized IdleTimeout")
	assert.Equal(t, 50, cfg.BatchConcurrency, "optimized BatchConcurrency")
	assert.Equal(t, 1000, cfg.TraceBatchSize, "optimized TraceBatchSize")
}

func TestOptimizedPoolTuningConfigIsValid(t *testing.T) {
	cfg := OptimizedPoolTuningConfig()
	assert.NoError(t, cfg.Validate(), "optimized config must pass validation")
}

// --- Validate ---------------------------------------------------------------

func TestValidateMaxConnectionsPerTarget(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	// Below minimum.
	cfg.MaxConnectionsPerTarget = 0
	assert.Error(t, cfg.Validate(), "MaxConnectionsPerTarget=0 should fail")

	// At minimum.
	cfg.MaxConnectionsPerTarget = 1
	assert.NoError(t, cfg.Validate(), "MaxConnectionsPerTarget=1 should pass")

	// At maximum.
	cfg.MaxConnectionsPerTarget = 50
	assert.NoError(t, cfg.Validate(), "MaxConnectionsPerTarget=50 should pass")

	// Above maximum.
	cfg.MaxConnectionsPerTarget = 51
	assert.Error(t, cfg.Validate(), "MaxConnectionsPerTarget=51 should fail")
}

func TestValidateMaxConcurrentTargets(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	cfg.MaxConcurrentTargets = 0
	assert.Error(t, cfg.Validate())

	cfg.MaxConcurrentTargets = 1
	assert.NoError(t, cfg.Validate())

	cfg.MaxConcurrentTargets = 500
	assert.NoError(t, cfg.Validate())

	cfg.MaxConcurrentTargets = 501
	assert.Error(t, cfg.Validate())
}

func TestValidateConnectionTimeout(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	// Below minimum.
	cfg.ConnectionTimeout = 500 * time.Millisecond
	assert.Error(t, cfg.Validate(), "ConnectionTimeout < 1s should fail")

	// At minimum.
	cfg.ConnectionTimeout = 1 * time.Second
	assert.NoError(t, cfg.Validate())

	// At maximum.
	cfg.ConnectionTimeout = 5 * time.Minute
	assert.NoError(t, cfg.Validate())

	// Above maximum.
	cfg.ConnectionTimeout = 6 * time.Minute
	assert.Error(t, cfg.Validate())
}

func TestValidateIdleTimeout(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	// Below minimum.
	cfg.IdleTimeout = 5 * time.Second
	assert.Error(t, cfg.Validate(), "IdleTimeout < 10s should fail")

	// At minimum.
	cfg.IdleTimeout = 10 * time.Second
	assert.NoError(t, cfg.Validate())

	// At maximum.
	cfg.IdleTimeout = 30 * time.Minute
	assert.NoError(t, cfg.Validate())

	// Above maximum.
	cfg.IdleTimeout = 31 * time.Minute
	assert.Error(t, cfg.Validate())
}

func TestValidateBatchConcurrency(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	cfg.BatchConcurrency = 0
	assert.Error(t, cfg.Validate())

	cfg.BatchConcurrency = 1
	assert.NoError(t, cfg.Validate())

	cfg.BatchConcurrency = 100
	assert.NoError(t, cfg.Validate())

	cfg.BatchConcurrency = 101
	assert.Error(t, cfg.Validate())
}

func TestValidateTraceBatchSize(t *testing.T) {
	cfg := DefaultPoolTuningConfig()

	cfg.TraceBatchSize = 9
	assert.Error(t, cfg.Validate())

	cfg.TraceBatchSize = 10
	assert.NoError(t, cfg.Validate())

	cfg.TraceBatchSize = 10000
	assert.NoError(t, cfg.Validate())

	cfg.TraceBatchSize = 10001
	assert.Error(t, cfg.Validate())
}

func TestValidateReportsFirstInvalidField(t *testing.T) {
	cfg := &PoolTuningConfig{
		MaxConnectionsPerTarget: 0, // invalid
		MaxConcurrentTargets:    0, // also invalid
		ConnectionTimeout:       30 * time.Second,
		IdleTimeout:             5 * time.Minute,
		BatchConcurrency:        10,
		TraceBatchSize:          100,
	}
	err := cfg.Validate()
	require.Error(t, err)
	// Should mention MaxConnectionsPerTarget (the first invalid field).
	assert.Contains(t, err.Error(), "MaxConnectionsPerTarget")
}

// --- ApplyOverrides ---------------------------------------------------------

func TestApplyOverridesSingleOption(t *testing.T) {
	base := DefaultPoolTuningConfig()
	derived, err := base.ApplyOverrides(WithMaxConcurrentTargets(50))
	require.NoError(t, err)

	assert.Equal(t, 50, derived.MaxConcurrentTargets, "overridden field")
	assert.Equal(t, base.MaxConnectionsPerTarget, derived.MaxConnectionsPerTarget,
		"non-overridden field must stay the same")
	assert.Equal(t, base.ConnectionTimeout, derived.ConnectionTimeout)
	assert.Equal(t, base.IdleTimeout, derived.IdleTimeout)
	assert.Equal(t, base.BatchConcurrency, derived.BatchConcurrency)
	assert.Equal(t, base.TraceBatchSize, derived.TraceBatchSize)

	// Original must not be modified.
	assert.Equal(t, 20, base.MaxConcurrentTargets,
		"ApplyOverrides must not mutate the receiver")
}

func TestApplyOverridesMultipleOptions(t *testing.T) {
	base := DefaultPoolTuningConfig()
	derived, err := base.ApplyOverrides(
		WithMaxConcurrentTargets(200),
		WithBatchConcurrency(40),
		WithTraceBatchSize(500),
	)
	require.NoError(t, err)

	assert.Equal(t, 200, derived.MaxConcurrentTargets)
	assert.Equal(t, 40, derived.BatchConcurrency)
	assert.Equal(t, 500, derived.TraceBatchSize)
}

func TestApplyOverridesInvalidReturnsError(t *testing.T) {
	base := DefaultPoolTuningConfig()
	_, err := base.ApplyOverrides(WithMaxConnectionsPerTarget(999))
	assert.Error(t, err, "invalid override should fail validation")
}

func TestApplyOverridesLaterOptionWins(t *testing.T) {
	base := DefaultPoolTuningConfig()
	derived, err := base.ApplyOverrides(
		WithMaxConcurrentTargets(50),
		WithMaxConcurrentTargets(75),
	)
	require.NoError(t, err)
	assert.Equal(t, 75, derived.MaxConcurrentTargets,
		"later option should override earlier one")
}

// --- functional options -----------------------------------------------------

func TestWithConnectionTimeout(t *testing.T) {
	cfg := DefaultPoolTuningConfig()
	derived, err := cfg.ApplyOverrides(WithConnectionTimeout(2 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 2*time.Minute, derived.ConnectionTimeout)
}

func TestWithIdleTimeout(t *testing.T) {
	cfg := DefaultPoolTuningConfig()
	derived, err := cfg.ApplyOverrides(WithIdleTimeout(15 * time.Minute))
	require.NoError(t, err)
	assert.Equal(t, 15*time.Minute, derived.IdleTimeout)
}

func TestWithMaxConnectionsPerTarget(t *testing.T) {
	cfg := DefaultPoolTuningConfig()
	derived, err := cfg.ApplyOverrides(WithMaxConnectionsPerTarget(25))
	require.NoError(t, err)
	assert.Equal(t, 25, derived.MaxConnectionsPerTarget)
}

// --- boundary values --------------------------------------------------------

func TestValidateAllFieldsAtMinimum(t *testing.T) {
	cfg := &PoolTuningConfig{
		MaxConnectionsPerTarget: 1,
		MaxConcurrentTargets:    1,
		ConnectionTimeout:       1 * time.Second,
		IdleTimeout:             10 * time.Second,
		BatchConcurrency:        1,
		TraceBatchSize:          10,
	}
	assert.NoError(t, cfg.Validate(), "all fields at minimum should pass")
}

func TestValidateAllFieldsAtMaximum(t *testing.T) {
	cfg := &PoolTuningConfig{
		MaxConnectionsPerTarget: 50,
		MaxConcurrentTargets:    500,
		ConnectionTimeout:       5 * time.Minute,
		IdleTimeout:             30 * time.Minute,
		BatchConcurrency:        100,
		TraceBatchSize:          10000,
	}
	assert.NoError(t, cfg.Validate(), "all fields at maximum should pass")
}
