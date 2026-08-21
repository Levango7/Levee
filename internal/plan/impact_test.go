package plan

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildPlan constructs a Plan directly (bypassing the Generator) with the
// given workflow name, a single batch holding the given targets, and the
// given steps. Used by impact tests to isolate analyzer behaviour from
// generator concerns.
func buildPlan(name string, targets []string, steps ...PlanStep) *Plan {
	return &Plan{
		ID:           "plan-test",
		WorkflowName: name,
		Batches: []Batch{
			{
				Index:   0,
				Targets: targets,
				Steps:   steps,
			},
		},
		TotalTargets: len(targets),
	}
}

// buildPlanBatches constructs a Plan with explicit batches.
func buildPlanBatches(name string, batches []Batch) *Plan {
	total := 0
	for _, b := range batches {
		total += len(b.Targets)
	}
	return &Plan{
		ID:           "plan-test",
		WorkflowName: name,
		Batches:      batches,
		TotalTargets: total,
	}
}

// TestAnalyzeSmallScale verifies a small plan (< 10 targets) yields a
// low risk level and the expected direct target list.
func TestAnalyzeSmallScale(t *testing.T) {
	targets := make([]string, 5)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%d", i)
	}
	plan := buildPlan("wf-small", targets)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Len(t, report.DirectTargets, 5)
	assert.Empty(t, report.IndirectTargets)
	assert.Equal(t, 5, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)

	// Direct targets are sorted.
	assert.Equal(t, []string{"host-0", "host-1", "host-2", "host-3", "host-4"}, report.DirectTargets)
}

// TestAnalyzeMediumScale verifies a medium plan (10-50 targets) yields a
// medium risk level.
func TestAnalyzeMediumScale(t *testing.T) {
	targets := make([]string, 25)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%02d", i)
	}
	plan := buildPlan("wf-medium", targets)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, 25, report.TotalAffected)
	assert.Equal(t, RiskLevelMedium, report.RiskLevel)
}

// TestAnalyzeLargeScale verifies a large plan (> 50 targets) yields a
// high risk level.
func TestAnalyzeLargeScale(t *testing.T) {
	targets := make([]string, 75)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%03d", i)
	}
	plan := buildPlan("wf-large", targets)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, 75, report.TotalAffected)
	assert.Equal(t, RiskLevelHigh, report.RiskLevel)
}

// TestAnalyzeRiskBoundaries verifies the risk level thresholds at the
// exact boundaries: 9 → low, 10 → medium, 50 → medium, 51 → high.
func TestAnalyzeRiskBoundaries(t *testing.T) {
	cases := []struct {
		total    int
		expected string
	}{
		{0, RiskLevelLow},
		{9, RiskLevelLow},
		{10, RiskLevelMedium},
		{50, RiskLevelMedium},
		{51, RiskLevelHigh},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("total=%d", c.total), func(t *testing.T) {
			targets := make([]string, c.total)
			for i := range targets {
				targets[i] = fmt.Sprintf("h-%d", i)
			}
			plan := buildPlan("wf-bound", targets)

			a := NewImpactAnalyzer()
			report := a.Analyze(plan)
			require.NotNil(t, report)
			assert.Equal(t, c.expected, report.RiskLevel)
			assert.Equal(t, c.total, report.TotalAffected)
		})
	}
}

// TestAnalyzeSingleTarget verifies a single-target plan.
func TestAnalyzeSingleTarget(t *testing.T) {
	plan := buildPlan("wf-single", []string{"lonely-host"})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, []string{"lonely-host"}, report.DirectTargets)
	assert.Empty(t, report.IndirectTargets)
	assert.Equal(t, 1, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)
}

// TestAnalyzeEmptyTargets verifies a plan with no batches and no targets
// produces an empty report with zero total and low risk.
func TestAnalyzeEmptyTargets(t *testing.T) {
	plan := &Plan{
		ID:           "plan-empty",
		WorkflowName: "wf-empty",
		Batches:      nil,
		TotalTargets: 0,
	}

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Empty(t, report.DirectTargets)
	assert.Empty(t, report.IndirectTargets)
	assert.Equal(t, 0, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)
}

// TestAnalyzeEmptyBatches verifies a plan with batches that have empty
// target slices still produces a valid empty report.
func TestAnalyzeEmptyBatches(t *testing.T) {
	plan := buildPlanBatches("wf-empty-batches", []Batch{
		{Index: 0, Targets: nil},
		{Index: 1, Targets: []string{}},
	})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Empty(t, report.DirectTargets)
	assert.Equal(t, 0, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)
}

// TestAnalyzeNilPlan verifies that a nil plan yields a nil report.
func TestAnalyzeNilPlan(t *testing.T) {
	a := NewImpactAnalyzer()
	report := a.Analyze(nil)
	assert.Nil(t, report)
}

// TestAnalyzeDirectTargetsDedup verifies that targets appearing in
// multiple batches are deduplicated in the direct target list.
func TestAnalyzeDirectTargetsDedup(t *testing.T) {
	plan := buildPlanBatches("wf-dedup", []Batch{
		{Index: 0, Targets: []string{"host-a", "host-b"}},
		{Index: 1, Targets: []string{"host-b", "host-c"}},
		{Index: 2, Targets: []string{"host-a", "host-c", "host-d"}},
	})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	// Deduplicated and sorted.
	assert.Equal(t, []string{"host-a", "host-b", "host-c", "host-d"}, report.DirectTargets)
	assert.Equal(t, 4, report.TotalAffected)
}

// TestAnalyzeIndirectTargets verifies that indirect targets declared in
// step Args are collected and exclude direct targets.
func TestAnalyzeIndirectTargets(t *testing.T) {
	steps := []PlanStep{
		{
			Name:   "upgrade",
			Module: "pkg",
			Action: "upgrade",
			Args: map[string]any{
				"indirect_targets": []string{"downstream-1", "downstream-2"},
			},
		},
	}
	plan := buildPlan("wf-indirect", []string{"host-a", "host-b"}, steps...)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, []string{"host-a", "host-b"}, report.DirectTargets)
	assert.Equal(t, []string{"downstream-1", "downstream-2"}, report.IndirectTargets)
	assert.Equal(t, 4, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)
}

// TestAnalyzeIndirectTargetsExcludeDirect verifies that an indirect
// target which is also a direct target is not double-counted.
func TestAnalyzeIndirectTargetsExcludeDirect(t *testing.T) {
	steps := []PlanStep{
		{
			Name:   "upgrade",
			Module: "pkg",
			Action: "upgrade",
			Args: map[string]any{
				// host-a is a direct target; host-x is not.
				"indirect_targets": []string{"host-a", "host-x"},
			},
		},
	}
	plan := buildPlan("wf-overlap", []string{"host-a", "host-b"}, steps...)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, []string{"host-a", "host-b"}, report.DirectTargets)
	assert.Equal(t, []string{"host-x"}, report.IndirectTargets)
	assert.Equal(t, 3, report.TotalAffected)
}

// TestAnalyzeIndirectTargetsDedup verifies that indirect targets
// declared across multiple steps and batches are deduplicated.
func TestAnalyzeIndirectTargetsDedup(t *testing.T) {
	stepA := PlanStep{
		Name:   "a",
		Module: "pkg",
		Action: "upgrade",
		Args: map[string]any{
			"indirect_targets": []string{"down-1", "down-2"},
		},
	}
	stepB := PlanStep{
		Name:   "b",
		Module: "pkg",
		Action: "upgrade",
		Args: map[string]any{
			"indirect_targets": []string{"down-2", "down-3"},
		},
	}
	plan := buildPlanBatches("wf-indirect-dedup", []Batch{
		{Index: 0, Targets: []string{"host-a"}, Steps: []PlanStep{stepA}},
		{Index: 1, Targets: []string{"host-b"}, Steps: []PlanStep{stepB}},
	})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, []string{"down-1", "down-2", "down-3"}, report.IndirectTargets)
	assert.Equal(t, 5, report.TotalAffected) // 2 direct + 3 indirect
}

// TestAnalyzeIndirectTargetsTypes verifies the different accepted shapes
// of the "indirect_targets" field: []string, []any of strings, and a
// single string.
func TestAnalyzeIndirectTargetsTypes(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{
			name: "string_slice",
			args: map[string]any{"indirect_targets": []string{"d-1", "d-2"}},
			want: []string{"d-1", "d-2"},
		},
		{
			name: "any_slice",
			args: map[string]any{"indirect_targets": []any{"d-1", "d-2"}},
			want: []string{"d-1", "d-2"},
		},
		{
			name: "single_string",
			args: map[string]any{"indirect_targets": "d-1"},
			want: []string{"d-1"},
		},
		{
			name: "any_slice_with_non_strings",
			args: map[string]any{"indirect_targets": []any{"d-1", 42, true, "d-2"}},
			want: []string{"d-1", "d-2"},
		},
		{
			name: "unknown_type",
			args: map[string]any{"indirect_targets": 42},
			want: []string{},
		},
		{
			name: "missing_field",
			args: map[string]any{"other": "value"},
			want: []string{},
		},
		{
			name: "nil_args",
			args: nil,
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			step := PlanStep{Name: "s", Module: "pkg", Action: "upgrade", Args: c.args}
			plan := buildPlan("wf-types", []string{"host-a"}, step)

			a := NewImpactAnalyzer()
			report := a.Analyze(plan)
			require.NotNil(t, report)
			assert.Equal(t, c.want, report.IndirectTargets)
		})
	}
}

// TestAnalyzeIndirectTargetsAffectRisk verifies that indirect targets
// count toward the total and can push the risk level up.
func TestAnalyzeIndirectTargetsAffectRisk(t *testing.T) {
	// 5 direct + 60 indirect = 65 total → high.
	indirect := make([]string, 60)
	for i := range indirect {
		indirect[i] = fmt.Sprintf("down-%d", i)
	}
	steps := []PlanStep{
		{
			Name:   "upgrade",
			Module: "pkg",
			Action: "upgrade",
			Args:   map[string]any{"indirect_targets": indirect},
		},
	}
	direct := []string{"h-0", "h-1", "h-2", "h-3", "h-4"}
	plan := buildPlan("wf-risk", direct, steps...)

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)

	assert.Equal(t, 5, len(report.DirectTargets))
	assert.Equal(t, 60, len(report.IndirectTargets))
	assert.Equal(t, 65, report.TotalAffected)
	assert.Equal(t, RiskLevelHigh, report.RiskLevel)
}

// TestAnalyzeDirectTargetsSorted verifies that the direct target list
// is sorted regardless of insertion order.
func TestAnalyzeDirectTargetsSorted(t *testing.T) {
	plan := buildPlan("wf-unsorted", []string{"zeta", "alpha", "mike", "bravo"})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)
	assert.Equal(t, []string{"alpha", "bravo", "mike", "zeta"}, report.DirectTargets)
}

// TestAnalyzeMultipleBatches verifies that targets across multiple
// batches are all collected as direct targets.
func TestAnalyzeMultipleBatches(t *testing.T) {
	plan := buildPlanBatches("wf-multi", []Batch{
		{Index: 0, Targets: []string{"h-0", "h-1"}},
		{Index: 1, Targets: []string{"h-2", "h-3"}},
		{Index: 2, Targets: []string{"h-4"}},
	})

	a := NewImpactAnalyzer()
	report := a.Analyze(plan)
	require.NotNil(t, report)
	assert.Equal(t, []string{"h-0", "h-1", "h-2", "h-3", "h-4"}, report.DirectTargets)
	assert.Equal(t, 5, report.TotalAffected)
}

// TestNewImpactAnalyzer verifies the constructor returns a non-nil
// analyzer.
func TestNewImpactAnalyzer(t *testing.T) {
	a := NewImpactAnalyzer()
	require.NotNil(t, a)
}
