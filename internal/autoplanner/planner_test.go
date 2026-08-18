package autoplanner

// planner_test.go exercises the AutoPlanner and RiskAssessor implemented in
// planner.go and risk_assessor.go. The tests use table-driven style with
// testify/require + assert and aim for 90%+ coverage.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/recommend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test fixtures ---------------------------------------------------------

// sampleYAML is a well-formed LEVEELang YAML draft with a steps block
// containing two steps. It is used by many tests as the WorkflowDraft of a
// recommendation.
const sampleYAML = `name: auto-fix-order-service
description: Restart order-service with larger heap
target:
  hosts:
    - order-service
window:
  duration: 30m
  approval: standard
batches:
  - name: batch-1
    targets: all
    steps:
      - name: restart-service
        module: svc
        action: restart
        args:
          service: order-service
      - name: verify-health
        module: shell
        action: run
        args:
          cmd: curl -sf http://order-service:8080/health
rollback:
  - name: rollback-restart-service
    module: svc
    action: restart
`

// noStepsYAML is a YAML draft without a steps block. parseSteps should fall
// back to a single synthetic review step.
const noStepsYAML = `name: manual-fix
description: Operator must inspect manually
target:
  hosts:
    - misc-host
`

// newRec builds a recommendation with the given risk level and the sample
// YAML draft. Fields not relevant to the test are left zero.
func newRec(risk recommend.RiskLevel) *recommend.Recommendation {
	return &recommend.Recommendation{
		ID:            "rec-001",
		DiagnosisID:   "diag-001",
		Target:        "order-service.prod",
		Summary:       "Restart order-service with larger heap",
		Approach:      "Increase -Xmx to 4g and restart",
		WorkflowDraft: sampleYAML,
		RiskLevel:     risk,
		Confidence:    0.85,
		PreConditions: []string{"confirm SSH access", "snapshot heap"},
		RollbackPlan:  "restart with previous -Xmx",
		CreatedAt:     time.Now().UTC(),
	}
}

// newPlanner returns an AutoPlanner with default configuration.
func newPlanner() *AutoPlanner {
	return NewAutoPlanner(AutoPlannerConfig{})
}

// --- NewAutoPlanner --------------------------------------------------------

func TestNewAutoPlanner_Defaults(t *testing.T) {
	p := NewAutoPlanner(AutoPlannerConfig{})

	require.NotNil(t, p)
	assert.NotNil(t, p.planGen, "planGen should default to a non-nil Generator")
	assert.NotNil(t, p.impactAna, "impactAna should default to a non-nil ImpactAnalyzer")
	assert.NotNil(t, p.riskAssess, "riskAssess should default to a non-nil RiskAssessor")
	assert.NotNil(t, p.log, "log should default to a non-nil logger")
}

func TestNewAutoPlanner_CustomConfig(t *testing.T) {
	pg := plan.NewGenerator()
	ia := plan.NewImpactAnalyzer()
	ra := NewRiskAssessor()

	p := NewAutoPlanner(AutoPlannerConfig{
		PlanGen:    pg,
		ImpactAna:  ia,
		RiskAssess: ra,
	})

	require.NotNil(t, p)
	assert.Same(t, pg, p.planGen, "custom PlanGen should be preserved")
	assert.Same(t, ia, p.impactAna, "custom ImpactAna should be preserved")
	assert.Same(t, ra, p.riskAssess, "custom RiskAssess should be preserved")
}

// --- RiskAssessor ----------------------------------------------------------

func TestNewRiskAssessor(t *testing.T) {
	r := NewRiskAssessor()
	require.NotNil(t, r)
	assert.NotNil(t, r.levelMgr, "levelMgr should be initialised")
}

func TestRiskAssessor_Assess_LowRisk(t *testing.T) {
	r := NewRiskAssessor()
	rec := newRec(recommend.RiskLow)

	a := r.Assess(rec)

	assert.Equal(t, recommend.RiskLow, a.RiskLevel)
	assert.Equal(t, approval.LevelStandard, a.ApprovalLevel)
	assert.True(t, a.CanAutoExecute, "low risk should allow auto-execute")
	assert.InDelta(t, 0.85, a.Confidence, 1e-9)
	assert.NotEmpty(t, a.Reasons)
}

func TestRiskAssessor_Assess_MediumRisk(t *testing.T) {
	r := NewRiskAssessor()
	rec := newRec(recommend.RiskMedium)

	a := r.Assess(rec)

	assert.Equal(t, recommend.RiskMedium, a.RiskLevel)
	assert.Equal(t, approval.LevelHigh, a.ApprovalLevel)
	assert.False(t, a.CanAutoExecute, "medium risk should not allow auto-execute")
}

func TestRiskAssessor_Assess_HighRisk(t *testing.T) {
	r := NewRiskAssessor()
	rec := newRec(recommend.RiskHigh)

	a := r.Assess(rec)

	assert.Equal(t, recommend.RiskHigh, a.RiskLevel)
	assert.Equal(t, approval.LevelHigh, a.ApprovalLevel)
	assert.False(t, a.CanAutoExecute, "high risk should not allow auto-execute")
}

func TestRiskAssessor_Assess_CriticalRisk(t *testing.T) {
	r := NewRiskAssessor()
	rec := newRec(recommend.RiskCritical)

	a := r.Assess(rec)

	assert.Equal(t, recommend.RiskCritical, a.RiskLevel)
	assert.Equal(t, approval.LevelEmergency, a.ApprovalLevel)
	assert.False(t, a.CanAutoExecute, "critical risk should not allow auto-execute")
}

func TestRiskAssessor_Assess_UnknownRisk(t *testing.T) {
	r := NewRiskAssessor()
	rec := newRec(recommend.RiskLevel("unknown"))

	a := r.Assess(rec)

	assert.Equal(t, recommend.RiskLevel("unknown"), a.RiskLevel)
	assert.Equal(t, approval.LevelHigh, a.ApprovalLevel,
		"unknown risk should default to high tier (conservative)")
	assert.False(t, a.CanAutoExecute)
}

func TestRiskAssessor_Assess_NilRecommendation(t *testing.T) {
	r := NewRiskAssessor()

	a := r.Assess(nil)

	assert.Equal(t, approval.LevelStandard, a.ApprovalLevel)
	assert.False(t, a.CanAutoExecute, "nil rec should never auto-execute")
	assert.NotEmpty(t, a.Reasons)
}

// Table-driven test covering all four risk levels plus the unknown case.
func TestRiskAssessor_Assess_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		risk      recommend.RiskLevel
		wantLevel string
		wantAuto  bool
	}{
		{"low", recommend.RiskLow, approval.LevelStandard, true},
		{"medium", recommend.RiskMedium, approval.LevelHigh, false},
		{"high", recommend.RiskHigh, approval.LevelHigh, false},
		{"critical", recommend.RiskCritical, approval.LevelEmergency, false},
		{"unknown", recommend.RiskLevel("bogus"), approval.LevelHigh, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRiskAssessor()
			rec := newRec(tc.risk)
			a := r.Assess(rec)
			assert.Equal(t, tc.wantLevel, a.ApprovalLevel)
			assert.Equal(t, tc.wantAuto, a.CanAutoExecute)
		})
	}
}

// --- Plan: error paths -----------------------------------------------------

func TestPlan_NilRecommendation(t *testing.T) {
	p := newPlanner()

	wf, err := p.Plan(context.Background(), nil)

	require.ErrorIs(t, err, ErrNilRecommendation)
	assert.Nil(t, wf)
}

func TestPlan_EmptyWorkflowDraft(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.WorkflowDraft = ""

	wf, err := p.Plan(context.Background(), rec)

	require.ErrorIs(t, err, ErrEmptyWorkflowDraft)
	assert.Nil(t, wf)
}

func TestPlan_WhitespaceOnlyWorkflowDraft(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.WorkflowDraft = "   \n\t\n  "

	wf, err := p.Plan(context.Background(), rec)

	require.ErrorIs(t, err, ErrEmptyWorkflowDraft)
	assert.Nil(t, wf)
}

func TestPlan_InvalidRiskLevel(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLevel("bogus"))

	wf, err := p.Plan(context.Background(), rec)

	require.ErrorIs(t, err, ErrInvalidRiskLevel)
	assert.Nil(t, wf)
	assert.Contains(t, err.Error(), "bogus")
}

func TestPlan_NilContext(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)

	// Plan should tolerate a nil context by substituting background.
	wf, err := p.Plan(nil, rec)

	require.NoError(t, err)
	require.NotNil(t, wf)
}

// --- Plan: success paths ---------------------------------------------------

func TestPlan_LowRisk(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)

	wf, err := p.Plan(context.Background(), rec)

	require.NoError(t, err)
	require.NotNil(t, wf)
	assert.Equal(t, recommend.RiskLow, wf.RiskLevel)
	assert.Equal(t, approval.LevelStandard, wf.ApprovalLevel)
}

func TestPlan_MediumRisk(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskMedium)

	wf, err := p.Plan(context.Background(), rec)

	require.NoError(t, err)
	require.NotNil(t, wf)
	assert.Equal(t, approval.LevelHigh, wf.ApprovalLevel)
}

func TestPlan_HighRisk(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskHigh)

	wf, err := p.Plan(context.Background(), rec)

	require.NoError(t, err)
	require.NotNil(t, wf)
	assert.Equal(t, approval.LevelHigh, wf.ApprovalLevel)
}

func TestPlan_CriticalRisk(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskCritical)

	wf, err := p.Plan(context.Background(), rec)

	require.NoError(t, err)
	require.NotNil(t, wf)
	assert.Equal(t, approval.LevelEmergency, wf.ApprovalLevel)
}

// Table-driven success test covering all four risk levels.
func TestPlan_AllRiskLevels_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		risk      recommend.RiskLevel
		wantLevel string
	}{
		{"low", recommend.RiskLow, approval.LevelStandard},
		{"medium", recommend.RiskMedium, approval.LevelHigh},
		{"high", recommend.RiskHigh, approval.LevelHigh},
		{"critical", recommend.RiskCritical, approval.LevelEmergency},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlanner()
			rec := newRec(tc.risk)
			wf, err := p.Plan(context.Background(), rec)
			require.NoError(t, err)
			require.NotNil(t, wf)
			assert.Equal(t, tc.wantLevel, wf.ApprovalLevel)
			assert.Equal(t, tc.risk, wf.RiskLevel)
		})
	}
}

// --- Plan: Workflow field integrity ----------------------------------------

func TestPlan_WorkflowFields(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)

	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)
	require.NotNil(t, wf)

	// ID should be a non-empty UUID-like string.
	assert.NotEmpty(t, wf.ID)
	assert.True(t, strings.Contains(wf.ID, "-"), "ID should look like a UUID")

	// Name should come from the recommendation summary.
	assert.Equal(t, rec.Summary, wf.Name)

	// YAML should contain the original draft plus the formalisation block.
	assert.Contains(t, wf.YAML, rec.WorkflowDraft)
	assert.Contains(t, wf.YAML, "autoplanner formalisation")
	assert.Contains(t, wf.YAML, "approval: standard")

	// Risk and approval tiers.
	assert.Equal(t, rec.RiskLevel, wf.RiskLevel)
	assert.Equal(t, approval.LevelStandard, wf.ApprovalLevel)

	// Operational metadata copied from the recommendation.
	assert.Equal(t, rec.PreConditions, wf.PreConditions)
	assert.Equal(t, rec.RollbackPlan, wf.RollbackPlan)
	assert.Equal(t, rec.Target, wf.Target)

	// Timestamp should be recent and UTC.
	assert.WithinDuration(t, time.Now().UTC(), wf.CreatedAt, 5*time.Second)

	// Batches: single target -> single batch.
	require.Len(t, wf.Batches, 1)
	assert.Equal(t, 1, wf.Batches[0].ID)
	assert.Equal(t, []string{rec.Target}, wf.Batches[0].Targets)

	// Estimated time: 2 steps * 30s = 60s.
	assert.Equal(t, 60*time.Second, wf.EstimatedTime)
}

func TestPlan_WorkflowName_FallbackToTarget(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.Summary = ""

	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)
	assert.Equal(t, "auto-fix-"+rec.Target, wf.Name)
}

func TestPlan_WorkflowName_FallbackToUnnamed(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.Summary = ""
	rec.Target = ""

	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)
	assert.Equal(t, "auto-fix-unnamed", wf.Name)
}

func TestPlan_YAMLFormalisation_NoTrailingNewline(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.WorkflowDraft = strings.TrimRight(sampleYAML, "\n")

	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)
	// The formalisation block should be on its own lines.
	assert.Contains(t, wf.YAML, "\n# --- autoplanner formalisation ---\n")
}

// --- parseSteps ------------------------------------------------------------

func TestParseSteps_WithStepsBlock(t *testing.T) {
	p := newPlanner()
	steps := p.parseSteps(sampleYAML)

	require.Len(t, steps, 2)
	assert.Equal(t, "restart-service", steps[0].Name)
	assert.Equal(t, "svc", steps[0].Module)
	assert.Equal(t, "restart", steps[0].Action)
	assert.Equal(t, "verify-health", steps[1].Name)
	assert.Equal(t, "shell", steps[1].Module)
	assert.Equal(t, "run", steps[1].Action)
}

func TestParseSteps_NoStepsBlock(t *testing.T) {
	p := newPlanner()
	steps := p.parseSteps(noStepsYAML)

	require.Len(t, steps, 1)
	assert.Equal(t, "review-draft", steps[0].Name)
	assert.Equal(t, "shell", steps[0].Module)
	assert.Equal(t, "run", steps[0].Action)
	assert.NotEmpty(t, steps[0].Args["draft"])
}

func TestParseSteps_EmptyYAML(t *testing.T) {
	p := newPlanner()
	steps := p.parseSteps("")

	require.Len(t, steps, 1)
	assert.Equal(t, "review-draft", steps[0].Name)
}

func TestParseSteps_StepsWithDefaults(t *testing.T) {
	p := newPlanner()
	// Steps without module/action should get shell/run defaults.
	yaml := `batches:
  - steps:
      - name: bare-step
`
	steps := p.parseSteps(yaml)
	require.Len(t, steps, 1)
	assert.Equal(t, "bare-step", steps[0].Name)
	assert.Equal(t, "shell", steps[0].Module, "missing module should default to shell")
	assert.Equal(t, "run", steps[0].Action, "missing action should default to run")
}

func TestParseSteps_StepWithTarget(t *testing.T) {
	p := newPlanner()
	yaml := `steps:
      - name: targeted
        module: svc
        action: restart
        target: db-host
`
	steps := p.parseSteps(yaml)
	require.Len(t, steps, 1)
	assert.Equal(t, "db-host", steps[0].OnTarget)
}

func TestParseSteps_CommentsAndBlankLines(t *testing.T) {
	p := newPlanner()
	yaml := `# leading comment

steps:
  # inner comment

      - name: step-one
        module: shell
        action: run

      - name: step-two
        module: svc
        action: restart
`
	steps := p.parseSteps(yaml)
	require.Len(t, steps, 2)
	assert.Equal(t, "step-one", steps[0].Name)
	assert.Equal(t, "step-two", steps[1].Name)
}

// --- divideBatches ---------------------------------------------------------

func TestDivideBatches_SingleTarget(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "host-01")

	require.Len(t, batches, 1)
	assert.Equal(t, 1, batches[0].ID)
	assert.Equal(t, []string{"host-01"}, batches[0].Targets)
	assert.Equal(t, steps, batches[0].Steps)
	assert.False(t, batches[0].Parallel, "single step should not be parallel")
}

func TestDivideBatches_MultipleStepsParallel(t *testing.T) {
	p := newPlanner()
	steps := []Step{
		{Name: "s1", Module: "shell", Action: "run"},
		{Name: "s2", Module: "svc", Action: "restart"},
	}

	batches := p.divideBatches(steps, "host-01")

	require.Len(t, batches, 1)
	assert.True(t, batches[0].Parallel, "multiple steps should be parallel")
}

func TestDivideBatches_MultipleTargetsSpaceSeparated(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "host-01 host-02 host-03")

	require.Len(t, batches, 3)
	assert.Equal(t, 1, batches[0].ID)
	assert.Equal(t, 2, batches[1].ID)
	assert.Equal(t, 3, batches[2].ID)
	assert.Equal(t, []string{"host-01"}, batches[0].Targets)
	assert.Equal(t, []string{"host-02"}, batches[1].Targets)
	assert.Equal(t, []string{"host-03"}, batches[2].Targets)
}

func TestDivideBatches_MultipleTargetsCommaSeparated(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "host-01,host-02")

	require.Len(t, batches, 2)
	assert.Equal(t, "host-01", batches[0].Targets[0])
	assert.Equal(t, "host-02", batches[1].Targets[0])
}

func TestDivideBatches_MixedSeparators(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "host-01, host-02 host-03")

	require.Len(t, batches, 3)
}

func TestDivideBatches_DuplicateTargets(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "host-01 host-01 host-02")

	require.Len(t, batches, 2, "duplicates should be removed")
	assert.Equal(t, "host-01", batches[0].Targets[0])
	assert.Equal(t, "host-02", batches[1].Targets[0])
}

func TestDivideBatches_EmptyTarget(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1", Module: "shell", Action: "run"}}

	batches := p.divideBatches(steps, "")

	require.Len(t, batches, 1)
	assert.Equal(t, []string{"all"}, batches[0].Targets, "empty target should yield 'all'")
}

func TestDivideBatches_NoSteps(t *testing.T) {
	p := newPlanner()

	batches := p.divideBatches(nil, "host-01")

	require.Len(t, batches, 1)
	assert.Empty(t, batches[0].Steps)
	assert.False(t, batches[0].Parallel)
}

// --- estimateTime ----------------------------------------------------------

func TestEstimateTime_ZeroSteps(t *testing.T) {
	p := newPlanner()
	assert.Equal(t, time.Duration(0), p.estimateTime(nil))
	assert.Equal(t, time.Duration(0), p.estimateTime([]Step{}))
}

func TestEstimateTime_SingleStep(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1"}}
	assert.Equal(t, 30*time.Second, p.estimateTime(steps))
}

func TestEstimateTime_MultipleSteps(t *testing.T) {
	p := newPlanner()
	steps := []Step{{Name: "s1"}, {Name: "s2"}, {Name: "s3"}}
	assert.Equal(t, 90*time.Second, p.estimateTime(steps))
}

func TestEstimateTime_MatchesBatchSteps(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)
	// sampleYAML has 2 steps -> 60s.
	assert.Equal(t, 60*time.Second, wf.EstimatedTime)
}

// --- splitTargets (covered via divideBatches but tested directly too) ------

func TestSplitTargets_Empty(t *testing.T) {
	assert.Nil(t, splitTargets(""))
}

func TestSplitTargets_Single(t *testing.T) {
	assert.Equal(t, []string{"host-01"}, splitTargets("host-01"))
}

func TestSplitTargets_Dedup(t *testing.T) {
	assert.Equal(t, []string{"a", "b"}, splitTargets("a a b a"))
}

// --- isValidRiskLevel ------------------------------------------------------

func TestIsValidRiskLevel(t *testing.T) {
	cases := []struct {
		level recommend.RiskLevel
		want  bool
	}{
		{recommend.RiskLow, true},
		{recommend.RiskMedium, true},
		{recommend.RiskHigh, true},
		{recommend.RiskCritical, true},
		{recommend.RiskLevel("bogus"), false},
		{recommend.RiskLevel(""), false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isValidRiskLevel(tc.level),
			"isValidRiskLevel(%q) should be %v", tc.level, tc.want)
	}
}

// --- workflowName ----------------------------------------------------------

func TestWorkflowName(t *testing.T) {
	cases := []struct {
		name string
		rec  *recommend.Recommendation
		want string
	}{
		{
			name: "with summary",
			rec:  &recommend.Recommendation{Summary: "fix it", Target: "host"},
			want: "fix it",
		},
		{
			name: "no summary, with target",
			rec:  &recommend.Recommendation{Target: "host-01"},
			want: "auto-fix-host-01",
		},
		{
			name: "no summary, no target",
			rec:  &recommend.Recommendation{},
			want: "auto-fix-unnamed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, workflowName(tc.rec))
		})
	}
}

// --- formaliseYAML (covered via Plan but tested directly too) --------------

func TestFormaliseYAML_ContainsFormalisationBlock(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	assessment := p.riskAssess.Assess(rec)

	yaml := p.formaliseYAML(rec, assessment, 60*time.Second)

	assert.Contains(t, yaml, rec.WorkflowDraft)
	assert.Contains(t, yaml, "# --- autoplanner formalisation ---")
	assert.Contains(t, yaml, "# approval: standard")
	assert.Contains(t, yaml, "# risk: low")
	assert.Contains(t, yaml, "# estimated_time: 1m0s")
}

// --- Integration: multi-target Plan ---------------------------------------

func TestPlan_MultiTargetBatches(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)
	rec.Target = "host-01 host-02 host-03"

	wf, err := p.Plan(context.Background(), rec)
	require.NoError(t, err)

	require.Len(t, wf.Batches, 3)
	assert.Equal(t, "host-01", wf.Batches[0].Targets[0])
	assert.Equal(t, "host-02", wf.Batches[1].Targets[0])
	assert.Equal(t, "host-03", wf.Batches[2].Targets[0])

	// Each batch should carry the same steps.
	for i, b := range wf.Batches {
		assert.NotEmpty(t, b.Steps, "batch %d should have steps", i)
	}
}

// --- Concurrency safety ----------------------------------------------------

func TestPlan_ConcurrentSafe(t *testing.T) {
	p := newPlanner()
	rec := newRec(recommend.RiskLow)

	const n = 20
	errs := make(chan error, n)
	workflows := make(chan *Workflow, n)

	for i := 0; i < n; i++ {
		go func() {
			wf, err := p.Plan(context.Background(), rec)
			errs <- err
			workflows <- wf
		}()
	}

	for i := 0; i < n; i++ {
		require.NoError(t, <-errs, "goroutine %d failed", i)
		wf := <-workflows
		require.NotNil(t, wf)
		assert.NotEmpty(t, wf.ID)
	}
}

// --- Error wrapping --------------------------------------------------------

func TestPlan_ErrorsAreSentinel(t *testing.T) {
	p := newPlanner()

	_, err := p.Plan(context.Background(), nil)
	assert.True(t, errors.Is(err, ErrNilRecommendation))

	rec := newRec(recommend.RiskLow)
	rec.WorkflowDraft = ""
	_, err = p.Plan(context.Background(), rec)
	assert.True(t, errors.Is(err, ErrEmptyWorkflowDraft))

	rec = newRec(recommend.RiskLevel("bogus"))
	_, err = p.Plan(context.Background(), rec)
	assert.True(t, errors.Is(err, ErrInvalidRiskLevel))
}
