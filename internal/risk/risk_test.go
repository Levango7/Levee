package risk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkStep builds a risk input step with the given knobs.
func mkStep(action string, irreversible, hasRollback bool) Step {
	return Step{Action: action, Irreversible: irreversible, HasRollback: hasRollback}
}

// mkInput assembles a single-batch input.
func mkInput(steps []Step, direct int) Input {
	return Input{Steps: steps, BatchCount: 1, DirectTargets: direct}
}

func TestEmptyInputIsStandardZero(t *testing.T) {
	a := NewAssessor().Assess(Input{})
	assert.Equal(t, 0, a.Score)
	assert.Equal(t, LevelStandard, a.ApprovalFloor)
}

func TestPlainReversiblePlanStaysStandard(t *testing.T) {
	// One host, one reversible step, rollback declared: nothing dangerous.
	a := NewAssessor().Assess(mkInput([]Step{mkStep("install", false, true)}, 1))
	assert.Equal(t, 0, a.Score, "factors: %v", a.Factors)
	assert.Equal(t, LevelStandard, a.ApprovalFloor)
	assert.Empty(t, a.Factors)
}

func TestIrreversibleStepForcesHighFloor(t *testing.T) {
	// R4 red line: ONE irreversible step on ONE host ⇒ at least high.
	a := NewAssessor().Assess(mkInput([]Step{mkStep("replica_switch", true, true)}, 1))
	require.Equal(t, 1, a.IrreversibleSteps)
	assert.Equal(t, LevelHigh, a.ApprovalFloor)
	var sawIRR bool
	for _, f := range a.Factors {
		if f.Rule == "irreversible_steps" {
			sawIRR = true
			assert.Equal(t, pointsIrreversibleStep, f.Points)
		}
	}
	assert.True(t, sawIRR)
}

func TestIrreversiblePlusWideBlastIsEmergency(t *testing.T) {
	// Irreversible + high-band blast (>50 affected): emergency.
	in := Input{
		Steps:           []Step{mkStep("drop_all", true, false)},
		BatchCount:      1,
		DirectTargets:   51,
		IndirectTargets: 0,
		BlastHigh:       true,
	}
	a := NewAssessor().Assess(in)
	assert.Equal(t, LevelEmergency, a.ApprovalFloor)
}

func TestDestructiveLexiconScoresWithoutDeclaration(t *testing.T) {
	// Defence in depth: a "purge"-named action the author did NOT
	// declare irreversible still contributes points.
	a := NewAssessor().Assess(mkInput([]Step{mkStep("cache.purge", false, true)}, 1))
	var sawDestructive bool
	for _, f := range a.Factors {
		if f.Rule == "destructive_actions" {
			sawDestructive = true
			assert.Equal(t, pointsDestructiveAction, f.Points)
		}
	}
	assert.True(t, sawDestructive)
	// 8 points stays in the standard band (floor override needs
	// irreversibility).
	assert.Equal(t, LevelStandard, a.ApprovalFloor)
}

func TestNoRollbackStepsPenalised(t *testing.T) {
	in := mkInput([]Step{
		mkStep("start", false, false),
		mkStep("stop", false, false),
		mkStep("restart", false, false),
	}, 1)
	a := NewAssessor().Assess(in)
	var sawRollbackFactor bool
	for _, f := range a.Factors {
		if f.Rule == "rollback_coverage" {
			sawRollbackFactor = true
			assert.Equal(t, 3*pointsNoRollbackStep, f.Points)
		}
	}
	assert.True(t, sawRollbackFactor)
}

func TestBlastRadiusBands(t *testing.T) {
	// 5 affected = low band: no points.
	a := NewAssessor().Assess(mkInput([]Step{mkStep("install", false, true)}, 5))
	for _, f := range a.Factors {
		assert.NotEqual(t, "blast_radius", f.Rule, "low band must not score")
	}

	// 20 affected = medium band: pointsPerBlastBand.
	a = NewAssessor().Assess(mkInput([]Step{mkStep("install", false, true)}, 20))
	var sawBlast bool
	for _, f := range a.Factors {
		if f.Rule == "blast_radius" {
			sawBlast = true
			assert.Equal(t, pointsPerBlastBand, f.Points)
		}
	}
	assert.True(t, sawBlast)

	// 60 affected = high band: 2 * pointsPerBlastBand.
	in := mkInput([]Step{mkStep("install", false, true)}, 60)
	in.BlastHigh = true
	a = NewAssessor().Assess(in)
	for _, f := range a.Factors {
		if f.Rule == "blast_radius" {
			assert.Equal(t, 2*pointsPerBlastBand, f.Points)
		}
	}
}

func TestBatchFanoutScores(t *testing.T) {
	in := Input{
		Steps:         []Step{mkStep("x", false, true)},
		BatchCount:    3,
		DirectTargets: 3,
	}
	a := NewAssessor().Assess(in)
	var sawFanout bool
	for _, f := range a.Factors {
		if f.Rule == "batch_fanout" {
			sawFanout = true
			assert.Equal(t, 2*pointsPerBatch, f.Points)
		}
	}
	assert.True(t, sawFanout)
}

func TestScoreClampedTo100(t *testing.T) {
	steps := make([]Step, 0, 10)
	for i := 0; i < 10; i++ {
		steps = append(steps, mkStep("delete", true, false))
	}
	a := NewAssessor().Assess(mkInput(steps, 1))
	assert.LessOrEqual(t, a.Score, 100)
	assert.Equal(t, LevelEmergency, a.ApprovalFloor)
}

func TestFactorsSortedDeterministically(t *testing.T) {
	in := mkInput([]Step{mkStep("delete", true, false)}, 20)
	a := NewAssessor().Assess(in)
	require.NotEmpty(t, a.Factors)
	for i := 1; i < len(a.Factors); i++ {
		assert.LessOrEqual(t, a.Factors[i-1].Rule, a.Factors[i].Rule)
	}
}

func TestMaxLevelOrdering(t *testing.T) {
	assert.Equal(t, LevelHigh, MaxLevel(LevelStandard, LevelHigh))
	assert.Equal(t, LevelHigh, MaxLevel(LevelHigh, LevelStandard))
	assert.Equal(t, LevelEmergency, MaxLevel(LevelHigh, LevelEmergency))
	assert.Equal(t, LevelStandard, MaxLevel(LevelStandard, LevelStandard))
	// Unknown strings degrade to standard rather than winning.
	assert.Equal(t, LevelHigh, MaxLevel("god-mode", LevelHigh))
	assert.Equal(t, LevelStandard, MaxLevel("weird", "legacy"))
}

func TestDestructiveActionMatching(t *testing.T) {
	assert.True(t, isDestructiveAction("remove"))
	assert.True(t, isDestructiveAction("pkg.remove"))
	assert.True(t, isDestructiveAction("mysql.drop_table"))
	assert.True(t, isDestructiveAction("FILE.DELETE"))
	assert.False(t, isDestructiveAction("install"))
	assert.False(t, isDestructiveAction("status.get"))
	assert.False(t, isDestructiveAction(""))
	// Substring containment is intentional (drop_all contains drop).
	assert.True(t, isDestructiveAction("drop_all"))
}
