package plan

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// makeDryRunWorkflow builds a workflow with static targets and the given
// batch config + steps. Used by dry-run tests to construct preview
// inputs without relying on the parser.
func makeDryRunWorkflow(name string, targets []string, batches dsl.BatchConfig, steps ...dsl.Step) *dsl.Workflow {
	return &dsl.Workflow{
		Meta:    dsl.WorkflowMeta{Name: name},
		Targets: []dsl.TargetGroup{{Name: "t", Hosts: targets}},
		Batches: batches,
		Steps:   steps,
	}
}

// newTestDryRunPreview returns a DryRunPreview backed by fresh
// Generator and ImpactAnalyzer instances.
func newTestDryRunPreview() *DryRunPreview {
	return NewDryRunPreview(NewGenerator(), NewImpactAnalyzer())
}

// TestPreviewBasic verifies that Preview returns a non-nil DryRunReport
// with the workflow name and at least one batch and one target.
func TestPreviewBasic(t *testing.T) {
	wf := makeDryRunWorkflow("wf-basic",
		[]string{"host-a", "host-b"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, "wf-basic", report.WorkflowName)
	assert.NotEmpty(t, report.Targets)
	assert.NotEmpty(t, report.Batches)
}

// TestPreviewTargets verifies that the report's Targets list matches the
// workflow's static hosts in declaration order.
func TestPreviewTargets(t *testing.T) {
	targets := []string{"host-1", "host-2", "host-3"}
	wf := makeDryRunWorkflow("wf-targets", targets,
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, targets, report.Targets)
}

// TestPreviewBatches verifies that the report's Batches list reflects
// the batch division produced by the Generator. With a percent strategy
// [50, 100] and 4 targets, there should be 2 batches of 2 targets each.
func TestPreviewBatches(t *testing.T) {
	targets := []string{"h-0", "h-1", "h-2", "h-3"}
	wf := makeDryRunWorkflow("wf-batches", targets,
		dsl.BatchConfig{Strategy: "percent", Steps: []int{50, 100}, MaxConcurrency: 2},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Batches, 2)
	assert.Equal(t, 0, report.Batches[0].Index)
	assert.Equal(t, 1, report.Batches[1].Index)
	assert.Len(t, report.Batches[0].Targets, 2)
	assert.Len(t, report.Batches[1].Targets, 2)
	// Step names propagated.
	assert.Equal(t, []string{"exec"}, report.Batches[0].Steps)
	assert.Equal(t, []string{"exec"}, report.Batches[1].Steps)
}

// TestPreviewImpact verifies that the report's Impact field is populated
// with the correct total target count and risk classification.
func TestPreviewImpact(t *testing.T) {
	targets := make([]string, 5)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%d", i)
	}
	wf := makeDryRunWorkflow("wf-impact", targets,
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, 5, report.Impact.TotalTargets)
	// 5 targets → low risk → all in LowRisk.
	assert.Len(t, report.Impact.LowRisk, 5)
	assert.Empty(t, report.Impact.MediumRisk)
	assert.Empty(t, report.Impact.HighRisk)
}

// TestPreviewImpactHighRisk verifies that a large plan (> 50 targets)
// classifies all direct targets as high-risk.
func TestPreviewImpactHighRisk(t *testing.T) {
	targets := make([]string, 60)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%03d", i)
	}
	wf := makeDryRunWorkflow("wf-high-risk", targets,
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, 60, report.Impact.TotalTargets)
	assert.Len(t, report.Impact.HighRisk, 60)
	assert.Empty(t, report.Impact.LowRisk)
}

// TestPreviewImpactIndirect verifies that indirect targets declared in
// step Args are classified as medium-risk.
func TestPreviewImpactIndirect(t *testing.T) {
	wf := makeDryRunWorkflow("wf-indirect", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{
			Name:   "upgrade",
			Module: "pkg",
			Action: "install",
			Args:   map[string]any{"indirect_targets": []string{"down-1", "down-2"}},
		},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	// 1 direct (low risk overall) + 2 indirect = 3 total.
	assert.Equal(t, 3, report.Impact.TotalTargets)
	assert.Len(t, report.Impact.LowRisk, 1)
	assert.Len(t, report.Impact.MediumRisk, 2)
	assert.Equal(t, []string{"down-1", "down-2"}, report.Impact.MediumRisk)
}

// TestPreviewEstDuration verifies that the estimated total duration is
// positive and matches the expected value for a known step type.
// shell.exec = 5s, single batch, single target → 5s total.
func TestPreviewEstDuration(t *testing.T) {
	wf := makeDryRunWorkflow("wf-dur", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.True(t, report.EstDuration > 0)
	assert.Equal(t, 5*time.Second, report.EstDuration)
}

// TestPreviewEstDurationMultipleSteps verifies that the duration of
// multiple sequential steps in a batch is the sum of step durations.
// shell.exec (5s) + file.copy (2s) = 7s.
func TestPreviewEstDurationMultipleSteps(t *testing.T) {
	wf := makeDryRunWorkflow("wf-multi-step", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
		dsl.Step{Name: "copy", Module: "file", Action: "copy"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, 7*time.Second, report.EstDuration)
}

// TestPreviewEstDurationUnknownStep verifies that unknown step types
// fall back to DefaultStepDuration (5s).
func TestPreviewEstDurationUnknownStep(t *testing.T) {
	wf := makeDryRunWorkflow("wf-unknown", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "custom", Module: "my", Action: "thing"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Equal(t, DefaultStepDuration, report.EstDuration)
}

// TestPreviewConflictsResourceContention verifies that a batch with
// unlimited concurrency (MaxConcurrency=0) over a large target set
// produces a resource_contention conflict.
func TestPreviewConflictsResourceContention(t *testing.T) {
	targets := make([]string, ResourceContentionThreshold+5)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%d", i)
	}
	// MaxConcurrency=0 → unlimited.
	wf := makeDryRunWorkflow("wf-contention", targets,
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.NotEmpty(t, report.Conflicts)
	found := false
	for _, c := range report.Conflicts {
		if c.Type == ConflictTypeResourceContention {
			found = true
			assert.Contains(t, c.Target, "batch-")
			break
		}
	}
	assert.True(t, found, "expected a resource_contention conflict")
}

// TestPreviewConflictsConcurrentEdit verifies that a batch with multiple
// write actions produces a concurrent_edit conflict.
func TestPreviewConflictsConcurrentEdit(t *testing.T) {
	wf := makeDryRunWorkflow("wf-concurrent-edit", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "copy", Module: "file", Action: "copy"},
		dsl.Step{Name: "template", Module: "file", Action: "template"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.NotEmpty(t, report.Conflicts)
	found := false
	for _, c := range report.Conflicts {
		if c.Type == ConflictTypeConcurrentEdit {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a concurrent_edit conflict")
}

// TestPreviewConflictsNone verifies that a simple plan with no conflicts
// produces an empty Conflicts list.
func TestPreviewConflictsNone(t *testing.T) {
	wf := makeDryRunWorkflow("wf-clean", []string{"host-a", "host-b"},
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 2},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Empty(t, report.Conflicts)
}

// TestPreviewNoSideEffects verifies that Preview is a pure read-only
// operation: calling it twice on the same workflow produces equivalent
// reports (same targets, batches, impact, duration, conflicts). Since
// DryRunPreview holds no state.Store, nothing is persisted.
func TestPreviewNoSideEffects(t *testing.T) {
	wf := makeDryRunWorkflow("wf-idempotent", []string{"host-a", "host-b"},
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 1},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()

	r1, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, r1)

	r2, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, r2)

	// Reports are equivalent on the deterministic fields.
	assert.Equal(t, r1.Targets, r2.Targets)
	assert.Equal(t, r1.WorkflowName, r2.WorkflowName)
	assert.Equal(t, r1.EstDuration, r2.EstDuration)
	assert.Equal(t, r1.Impact.TotalTargets, r2.Impact.TotalTargets)
	assert.Equal(t, len(r1.Batches), len(r2.Batches))
	assert.Equal(t, r1.Conflicts, r2.Conflicts)
}

// TestPreviewNilWorkflow verifies that a nil workflow returns
// ErrDryRunFailed.
func TestPreviewNilWorkflow(t *testing.T) {
	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, report)
	assert.True(t, errors.Is(err, ErrDryRunFailed))
}

// TestPreviewEmptyTargets verifies that a workflow with no static
// targets returns ErrDryRunFailed.
func TestPreviewEmptyTargets(t *testing.T) {
	wf := &dsl.Workflow{
		Meta:    dsl.WorkflowMeta{Name: "wf-empty"},
		Targets: nil,
		Steps:   []dsl.Step{{Name: "exec", Module: "shell", Action: "exec"}},
	}

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.Error(t, err)
	assert.Nil(t, report)
	assert.True(t, errors.Is(err, ErrDryRunFailed))
}

// TestPreviewCancelledContext verifies that a cancelled context returns
// ErrDryRunFailed.
func TestPreviewCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	wf := makeDryRunWorkflow("wf-cancel", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(ctx, wf)
	require.Error(t, err)
	assert.Nil(t, report)
	assert.True(t, errors.Is(err, ErrDryRunFailed))
}

// TestPreviewMultipleBatchesSerialDuration verifies that the total
// duration across multiple batches is the sum of per-batch durations
// (batches run sequentially).
func TestPreviewMultipleBatchesSerialDuration(t *testing.T) {
	targets := []string{"h-0", "h-1", "h-2", "h-3"}
	wf := makeDryRunWorkflow("wf-multi-batch", targets,
		dsl.BatchConfig{Strategy: "percent", Steps: []int{50, 100}, MaxConcurrency: 2},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Batches, 2)
	// Each batch: shell.exec = 5s. Two batches serial → 10s total.
	assert.Equal(t, 5*time.Second, report.Batches[0].EstDuration)
	assert.Equal(t, 5*time.Second, report.Batches[1].EstDuration)
	assert.Equal(t, 10*time.Second, report.EstDuration)
}

// TestPreviewBatchConcurrencyMax verifies that in-batch concurrency takes
// the maximum single-target duration rather than summing across targets.
// A batch with 4 targets each running shell.exec (5s) should have a
// batch duration of 5s, not 20s.
func TestPreviewBatchConcurrencyMax(t *testing.T) {
	targets := []string{"h-0", "h-1", "h-2", "h-3"}
	wf := makeDryRunWorkflow("wf-concurrency", targets,
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 4},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Batches, 1)
	batch := report.Batches[0]
	assert.Len(t, batch.Targets, 4)
	// Batch duration = max single-target duration = 5s (not 4*5=20s).
	assert.Equal(t, 5*time.Second, batch.EstDuration)
	assert.Equal(t, 5*time.Second, report.EstDuration)
}

// TestPreviewBatchConcurrencyMaxMultipleSteps verifies that with multiple
// steps the per-target duration is the step sum, and the batch duration
// is that sum (not multiplied by target count).
// shell.exec (5s) + pkg.install (30s) = 35s per target; 3 targets
// concurrent → batch = 35s.
func TestPreviewBatchConcurrencyMaxMultipleSteps(t *testing.T) {
	targets := []string{"h-0", "h-1", "h-2"}
	wf := makeDryRunWorkflow("wf-concurrency-multi", targets,
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 3},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
		dsl.Step{Name: "install", Module: "pkg", Action: "install"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Batches, 1)
	// 5s + 30s = 35s per target; 3 targets concurrent → 35s batch.
	assert.Equal(t, 35*time.Second, report.EstDuration)
}

// TestPreviewWarningsNoRollback verifies that a workflow without a
// rollback plan produces a warning.
func TestPreviewWarningsNoRollback(t *testing.T) {
	wf := makeDryRunWorkflow("wf-no-rollback", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 1},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)
	// wf.Rollback is nil by default.

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.NotEmpty(t, report.Warnings)
	assert.Contains(t, report.Warnings[0], "rollback")
}

// TestPreviewWarningsUnlimitedConcurrency verifies that a batch with
// unlimited concurrency and more than one target produces a warning.
func TestPreviewWarningsUnlimitedConcurrency(t *testing.T) {
	wf := makeDryRunWorkflow("wf-unlimited", []string{"h-0", "h-1"},
		dsl.BatchConfig{Strategy: "serial"}, // MaxConcurrency=0
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)
	wf.Rollback = &dsl.RollbackSpec{Strategy: "snapshot"}

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.NotEmpty(t, report.Warnings)
	assert.Contains(t, report.Warnings[0], "unlimited concurrency")
}

// TestPreviewWarningsNone verifies that a well-formed workflow produces
// no warnings.
func TestPreviewWarningsNone(t *testing.T) {
	wf := makeDryRunWorkflow("wf-clean", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial", MaxConcurrency: 1},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)
	wf.Rollback = &dsl.RollbackSpec{Strategy: "snapshot"}

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	assert.Empty(t, report.Warnings)
}

// TestPreviewFixedStrategy verifies that the fixed batch strategy
// produces the expected batch division in the preview.
func TestPreviewFixedStrategy(t *testing.T) {
	targets := []string{"h-0", "h-1", "h-2", "h-3", "h-4"}
	wf := makeDryRunWorkflow("wf-fixed", targets,
		dsl.BatchConfig{Strategy: "fixed", Steps: []int{2, 3}, MaxConcurrency: 2},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	require.Len(t, report.Batches, 2)
	assert.Len(t, report.Batches[0].Targets, 2)
	assert.Len(t, report.Batches[1].Targets, 3)
}

// TestPreviewAllStepTypes verifies the duration lookup for every known
// step type.
func TestPreviewAllStepTypes(t *testing.T) {
	cases := []struct {
		name     string
		module   string
		action   string
		expected time.Duration
	}{
		{"shell_exec", "shell", "exec", 5 * time.Second},
		{"file_copy", "file", "copy", 2 * time.Second},
		{"file_template", "file", "template", 2 * time.Second},
		{"pkg_install", "pkg", "install", 30 * time.Second},
		{"svc_manage", "svc", "manage", 5 * time.Second},
		{"user_manage", "user", "manage", 3 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wf := makeDryRunWorkflow("wf-step", []string{"host-a"},
				dsl.BatchConfig{Strategy: "serial"},
				dsl.Step{Name: c.name, Module: c.module, Action: c.action},
			)
			p := newTestDryRunPreview()
			report, err := p.Preview(context.Background(), wf)
			require.NoError(t, err)
			assert.Equal(t, c.expected, report.EstDuration)
		})
	}
}

// TestNewDryRunPreview verifies the constructor returns a non-nil
// previewer.
func TestNewDryRunPreview(t *testing.T) {
	p := NewDryRunPreview(NewGenerator(), NewImpactAnalyzer())
	require.NotNil(t, p)
}

// TestNewDryRunPreviewNilDeps verifies that a previewer built with nil
// dependencies returns ErrDryRunFailed on Preview.
func TestNewDryRunPreviewNilDeps(t *testing.T) {
	p := NewDryRunPreview(nil, nil)
	wf := makeDryRunWorkflow("wf-nil-deps", []string{"host-a"},
		dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "exec", Module: "shell", Action: "exec"},
	)
	report, err := p.Preview(context.Background(), wf)
	require.Error(t, err)
	assert.Nil(t, report)
	assert.True(t, errors.Is(err, ErrDryRunFailed))
}

// TestPreviewEmptyBatchDuration verifies that a batch with no targets
// has zero duration. This is a defensive case: the Generator normally
// does not produce empty batches, but the estimator should handle it.
func TestPreviewEmptyBatchDuration(t *testing.T) {
	b := Batch{Index: 0, Targets: nil, Steps: []PlanStep{{Name: "s", Module: "shell", Action: "exec"}}}
	assert.Equal(t, time.Duration(0), estimateBatchDuration(b))
}

// TestPreviewConflictsSorted verifies that the conflicts list is sorted
// by (Type, Target) for stable output.
func TestPreviewConflictsSorted(t *testing.T) {
	// Construct a plan with multiple conflict types via a workflow that
	// has multiple write steps and unlimited concurrency over a large
	// target set.
	targets := make([]string, ResourceContentionThreshold+5)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%d", i)
	}
	wf := makeDryRunWorkflow("wf-sorted", targets,
		dsl.BatchConfig{Strategy: "serial"}, // MaxConcurrency=0
		dsl.Step{Name: "copy", Module: "file", Action: "copy"},
		dsl.Step{Name: "template", Module: "file", Action: "template"},
	)

	p := newTestDryRunPreview()
	report, err := p.Preview(context.Background(), wf)
	require.NoError(t, err)
	require.NotNil(t, report)

	// Verify sorted by Type.
	for i := 1; i < len(report.Conflicts); i++ {
		assert.LessOrEqual(t, report.Conflicts[i-1].Type, report.Conflicts[i].Type,
			"conflicts should be sorted by type")
	}
}
