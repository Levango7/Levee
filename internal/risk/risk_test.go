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

// TestAutoApproveForbidden pins the tier gate ApplyChange / RollbackChange
// consult before honouring --force. The predicate had no test in its own
// package: the invariant was guarded only from the consumer side
// (internal/grpc/change_service.go:844, :1572), so a branch edit here turned
// no test in this package red.
func TestAutoApproveForbidden(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  bool
	}{
		{"empty is the unscored bootstrap path", "", false},
		{"standard may auto-approve", LevelStandard, false},
		{"high may not", LevelHigh, true},
		{"emergency may not", LevelEmergency, true},
		// Priority vocabulary: a run whose approval service never started
		// carries these in ApprovalLevel (see the P1-3 note on the func).
		// Reading them as tiers would deny auto-approve to every ordinary
		// "normal" change — the regression one fail-closed sweep caused once.
		{"priority low is not a tier", "low", false},
		{"priority normal is not a tier", "normal", false},
		{"priority urgent is not a tier", "urgent", false},
		// Fail-closed for anything unrecognised, including case variants and
		// vocabulary a future release might add.
		{"uppercase HIGH fails closed", "HIGH", true},
		{"capitalised Emergency fails closed", "Emergency", true},
		{"unknown tier fails closed", "critical", true},
		{"leading space fails closed", " high", true},
		{"blank string fails closed", " ", true},
		{"arbitrary string fails closed", "god-mode", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, AutoApproveForbidden(tc.level))
		})
	}
}

// TestAutoApproveForbiddenCoversMaxLevelOutput: whatever MaxLevel can return
// must be gated consistently — the two tiers above standard are always
// forbidden, standard never is. Pairs the two functions so a new tier added to
// one without the other shows up here.
func TestAutoApproveForbiddenCoversMaxLevelOutput(t *testing.T) {
	assert.False(t, AutoApproveForbidden(MaxLevel(LevelStandard, LevelStandard)))
	assert.True(t, AutoApproveForbidden(MaxLevel(LevelStandard, LevelHigh)))
	assert.True(t, AutoApproveForbidden(MaxLevel(LevelHigh, LevelEmergency)))
}
