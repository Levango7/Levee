package plan

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeWorkflow builds a minimal workflow for testing with the given
// batch config and steps. The targets field is populated with a
// placeholder so that the workflow passes structural validation; the
// real target list is supplied to Generate as resolvedTargets.
func makeWorkflow(name string, batches dsl.BatchConfig, steps ...dsl.Step) *dsl.Workflow {
	return &dsl.Workflow{
		Meta:    dsl.WorkflowMeta{Name: name},
		Targets: []dsl.TargetGroup{{Name: "t", Hosts: []string{"placeholder"}}},
		Batches: batches,
		Steps:   steps,
	}
}

// makeTargets returns a slice of target host names of the given size,
// formatted as host-000, host-001, ...
func makeTargets(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("host-%03d", i)
	}
	return out
}

// collectAllTargets gathers every target across all batches into a
// single slice, preserving order. Used to verify total coverage.
func collectAllTargets(batches []Batch) []string {
	var out []string
	for _, b := range batches {
		out = append(out, b.Targets...)
	}
	return out
}

// TestGeneratePercentStrategy verifies that the percent strategy
// divides targets according to the percentage milestones. With
// steps=[1,10,50,100] and 100 targets the batch sizes must be
// 1, 9, 40, 50.
func TestGeneratePercentStrategy(t *testing.T) {
	steps := []dsl.Step{
		{Name: "upgrade", Module: "pkg", Action: "upgrade"},
	}
	wf := makeWorkflow("wf-percent", dsl.BatchConfig{
		Strategy:       "percent",
		Steps:          []int{1, 10, 50, 100},
		MaxConcurrency: 10,
	}, steps...)
	targets := makeTargets(100)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.NotNil(t, plan)

	assert.Equal(t, "wf-percent", plan.WorkflowName)
	assert.Equal(t, 100, plan.TotalTargets)
	require.Len(t, plan.Batches, 4)

	// Batch sizes: 1%, 9%, 40%, 50%.
	assert.Len(t, plan.Batches[0].Targets, 1, "first batch should be 1%%")
	assert.Len(t, plan.Batches[1].Targets, 9, "second batch should be 9%%")
	assert.Len(t, plan.Batches[2].Targets, 40, "third batch should be 40%%")
	assert.Len(t, plan.Batches[3].Targets, 50, "fourth batch should be 50%%")

	// Indices are 0-based consecutive.
	for i, b := range plan.Batches {
		assert.Equal(t, i, b.Index, "batch index")
	}

	// MaxConcurrency propagated.
	for _, b := range plan.Batches {
		assert.Equal(t, 10, b.MaxConcurrency)
	}

	// Total coverage: no target dropped, no duplicate.
	all := collectAllTargets(plan.Batches)
	assert.Len(t, all, 100)
	seen := make(map[string]bool, 100)
	for _, h := range all {
		require.False(t, seen[h], "duplicate target %q", h)
		seen[h] = true
	}
}

// TestGeneratePercentRounding verifies that the percent strategy
// absorbs rounding error in the last batch. With 7 targets and
// steps=[1,10,50,100] the integer division produces cumulative
// counts 0, 0, 3, 7 — the last batch takes the remainder.
func TestGeneratePercentRounding(t *testing.T) {
	wf := makeWorkflow("wf-round", dsl.BatchConfig{
		Strategy: "percent",
		Steps:    []int{1, 10, 50, 100},
	})
	targets := makeTargets(7)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)

	// All 7 targets must be covered.
	all := collectAllTargets(plan.Batches)
	assert.Len(t, all, 7)

	// No empty batches.
	for i, b := range plan.Batches {
		require.NotEmpty(t, b.Targets, "batch %d should not be empty", i)
	}

	// No duplicates.
	seen := make(map[string]bool)
	for _, h := range all {
		require.False(t, seen[h], "duplicate %q", h)
		seen[h] = true
	}
}

// TestGenerateFixedStrategy verifies that the fixed strategy divides
// targets into batches of the declared sizes.
func TestGenerateFixedStrategy(t *testing.T) {
	wf := makeWorkflow("wf-fixed", dsl.BatchConfig{
		Strategy:       "fixed",
		Steps:          []int{2, 3, 5},
		MaxConcurrency: 4,
	})
	targets := makeTargets(10)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.Len(t, plan.Batches, 3)

	assert.Len(t, plan.Batches[0].Targets, 2)
	assert.Len(t, plan.Batches[1].Targets, 3)
	assert.Len(t, plan.Batches[2].Targets, 5)

	// Total coverage.
	all := collectAllTargets(plan.Batches)
	assert.Len(t, all, 10)
}

// TestGenerateFixedLeftover verifies that leftover targets beyond the
// declared fixed steps are placed in a trailing batch.
func TestGenerateFixedLeftover(t *testing.T) {
	wf := makeWorkflow("wf-leftover", dsl.BatchConfig{
		Strategy: "fixed",
		Steps:    []int{2, 3},
	})
	targets := makeTargets(8) // 2 + 3 = 5, leftover 3

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.Len(t, plan.Batches, 3)

	assert.Len(t, plan.Batches[0].Targets, 2)
	assert.Len(t, plan.Batches[1].Targets, 3)
	assert.Len(t, plan.Batches[2].Targets, 3, "leftover batch")

	all := collectAllTargets(plan.Batches)
	assert.Len(t, all, 8)
}

// TestGenerateFixedLastShort verifies that the last declared fixed
// batch is shorter than its size when targets run out.
func TestGenerateFixedLastShort(t *testing.T) {
	wf := makeWorkflow("wf-short", dsl.BatchConfig{
		Strategy: "fixed",
		Steps:    []int{2, 3, 5},
	})
	targets := makeTargets(6) // 2 + 3 + 1 (last short)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.Len(t, plan.Batches, 3)

	assert.Len(t, plan.Batches[0].Targets, 2)
	assert.Len(t, plan.Batches[1].Targets, 3)
	assert.Len(t, plan.Batches[2].Targets, 1, "last batch short")

	all := collectAllTargets(plan.Batches)
	assert.Len(t, all, 6)
}

// TestGenerateSerialStrategy verifies that the serial strategy puts
// all targets in a single batch.
func TestGenerateSerialStrategy(t *testing.T) {
	wf := makeWorkflow("wf-serial", dsl.BatchConfig{
		Strategy:       "serial",
		MaxConcurrency: 1,
	})
	targets := makeTargets(15)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	assert.Len(t, plan.Batches[0].Targets, 15)
	assert.Equal(t, 0, plan.Batches[0].Index)
}

// TestGenerateDefaultStrategy verifies that an empty strategy
// defaults to serial (all targets in one batch).
func TestGenerateDefaultStrategy(t *testing.T) {
	wf := makeWorkflow("wf-default", dsl.BatchConfig{})
	targets := makeTargets(5)

	g := NewGenerator()
	plan, err := g.Generate(wf, targets)
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	assert.Len(t, plan.Batches[0].Targets, 5)
}

// TestGenerateMultiStep verifies that multiple workflow steps are
// orchestrated into each batch in order.
func TestGenerateMultiStep(t *testing.T) {
	steps := []dsl.Step{
		{Name: "upgrade", Module: "pkg", Action: "upgrade"},
		{Name: "verify", Module: "shell", Action: "exec"},
		{Name: "restart", Module: "svc", Action: "restart"},
	}
	wf := makeWorkflow("wf-multistep", dsl.BatchConfig{Strategy: "serial"}, steps...)

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	require.Len(t, plan.Batches[0].Steps, 3)

	assert.Equal(t, "upgrade", plan.Batches[0].Steps[0].Name)
	assert.Equal(t, "verify", plan.Batches[0].Steps[1].Name)
	assert.Equal(t, "restart", plan.Batches[0].Steps[2].Name)

	// Module/action preserved.
	assert.Equal(t, "pkg", plan.Batches[0].Steps[0].Module)
	assert.Equal(t, "upgrade", plan.Batches[0].Steps[0].Action)
	assert.Equal(t, "shell", plan.Batches[0].Steps[1].Module)
	assert.Equal(t, "exec", plan.Batches[0].Steps[1].Action)
}

// TestGenerateEmptyTargets verifies that an empty resolved target list
// produces an error.
func TestGenerateEmptyTargets(t *testing.T) {
	wf := makeWorkflow("wf-empty", dsl.BatchConfig{Strategy: "serial"})

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{})
	require.Error(t, err)
	require.Nil(t, plan)
	assert.Contains(t, err.Error(), "LE092")
}

// TestGenerateNilTargets verifies that a nil resolved target list
// produces an error.
func TestGenerateNilTargets(t *testing.T) {
	wf := makeWorkflow("wf-nil", dsl.BatchConfig{Strategy: "serial"})

	g := NewGenerator()
	plan, err := g.Generate(wf, nil)
	require.Error(t, err)
	require.Nil(t, plan)
}

// TestGenerateNilWorkflow verifies that a nil workflow produces an
// error.
func TestGenerateNilWorkflow(t *testing.T) {
	g := NewGenerator()
	plan, err := g.Generate(nil, []string{"host-a"})
	require.Error(t, err)
	require.Nil(t, plan)
	assert.Contains(t, err.Error(), "LE002")
}

// TestGenerateSingleTarget verifies that a single target produces a
// single batch with one target.
func TestGenerateSingleTarget(t *testing.T) {
	wf := makeWorkflow("wf-single", dsl.BatchConfig{
		Strategy:       "percent",
		Steps:          []int{1, 10, 50, 100},
		MaxConcurrency: 5,
	})

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"lonely-host"})
	require.NoError(t, err)
	assert.Equal(t, 1, plan.TotalTargets)
	require.Len(t, plan.Batches, 1)
	assert.Len(t, plan.Batches[0].Targets, 1)
	assert.Equal(t, "lonely-host", plan.Batches[0].Targets[0])
}

// TestGenerateTotalCoveragePercent verifies that the percent strategy
// covers all targets without omission or duplication across a range of
// target counts (including counts that trigger rounding).
func TestGenerateTotalCoveragePercent(t *testing.T) {
	counts := []int{1, 2, 3, 7, 10, 33, 50, 99, 100, 101, 250}
	for _, n := range counts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			wf := makeWorkflow("wf-cov", dsl.BatchConfig{
				Strategy: "percent",
				Steps:    []int{1, 10, 50, 100},
			})
			targets := makeTargets(n)

			g := NewGenerator()
			plan, err := g.Generate(wf, targets)
			require.NoError(t, err)

			all := collectAllTargets(plan.Batches)
			assert.Len(t, all, n, "total targets covered")

			seen := make(map[string]bool, n)
			for _, h := range all {
				require.False(t, seen[h], "duplicate %q (n=%d)", h, n)
				seen[h] = true
			}
		})
	}
}

// TestGenerateTotalCoverageFixed verifies that the fixed strategy
// covers all targets without omission or duplication across a range of
// target counts.
func TestGenerateTotalCoverageFixed(t *testing.T) {
	counts := []int{1, 2, 3, 5, 10, 11, 20, 100}
	for _, n := range counts {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			wf := makeWorkflow("wf-cov", dsl.BatchConfig{
				Strategy: "fixed",
				Steps:    []int{2, 3, 5},
			})
			targets := makeTargets(n)

			g := NewGenerator()
			plan, err := g.Generate(wf, targets)
			require.NoError(t, err)

			all := collectAllTargets(plan.Batches)
			assert.Len(t, all, n, "total targets covered")

			seen := make(map[string]bool, n)
			for _, h := range all {
				require.False(t, seen[h], "duplicate %q (n=%d)", h, n)
				seen[h] = true
			}
		})
	}
}

// TestGenerateStepOverrides verifies that step-level rollback, approval
// and gate are propagated to the corresponding PlanStep.
func TestGenerateStepOverrides(t *testing.T) {
	rollback := &dsl.RollbackSpec{Strategy: "snapshot", OnFailure: "auto"}
	approval := &dsl.ApprovalSpec{Level: "high", MinApprovers: 2}
	gate := &dsl.GateSpec{Post: []dsl.GateCheck{
		{Type: "cmd", Command: "uname -r", ExpectExit: 0},
	}}
	args := map[string]any{"name": "kernel", "version": "5.15.0"}

	steps := []dsl.Step{
		{
			Name:     "upgrade",
			Module:   "pkg",
			Action:   "upgrade",
			Args:     args,
			Rollback: rollback,
			Approval: approval,
			Gate:     gate,
		},
	}
	wf := makeWorkflow("wf-overrides", dsl.BatchConfig{Strategy: "serial"}, steps...)

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	require.Len(t, plan.Batches[0].Steps, 1)

	ps := plan.Batches[0].Steps[0]
	assert.Equal(t, "upgrade", ps.Name)
	assert.Equal(t, "pkg", ps.Module)
	assert.Equal(t, "upgrade", ps.Action)
	assert.Equal(t, args, ps.Args)

	// Override pointers propagated.
	require.NotNil(t, ps.Rollback)
	assert.Equal(t, "snapshot", ps.Rollback.Strategy)
	assert.Equal(t, "auto", ps.Rollback.OnFailure)

	require.NotNil(t, ps.Approval)
	assert.Equal(t, "high", ps.Approval.Level)
	assert.Equal(t, 2, ps.Approval.MinApprovers)

	require.NotNil(t, ps.Gate)
	require.Len(t, ps.Gate.Post, 1)
	assert.Equal(t, "cmd", ps.Gate.Post[0].Type)
	assert.Equal(t, "uname -r", ps.Gate.Post[0].Command)
	assert.Equal(t, 0, ps.Gate.Post[0].ExpectExit)
}

// TestGenerateStepOverridesNil verifies that steps without overrides
// produce PlanSteps with nil rollback/approval/gate.
func TestGenerateStepOverridesNil(t *testing.T) {
	steps := []dsl.Step{
		{Name: "simple", Module: "shell", Action: "exec"},
	}
	wf := makeWorkflow("wf-nil-overrides", dsl.BatchConfig{Strategy: "serial"}, steps...)

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.Len(t, plan.Batches, 0+1)
	require.Len(t, plan.Batches[0].Steps, 1)

	ps := plan.Batches[0].Steps[0]
	assert.Nil(t, ps.Rollback)
	assert.Nil(t, ps.Approval)
	assert.Nil(t, ps.Gate)
}

// TestGeneratePlanID verifies that the plan ID is non-empty and has
// the expected prefix.
func TestGeneratePlanID(t *testing.T) {
	wf := makeWorkflow("wf-id", dsl.BatchConfig{Strategy: "serial"})

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	assert.NotEmpty(t, plan.ID)
	assert.True(t, strings.HasPrefix(plan.ID, "plan-"))
}

// TestGenerateCreatedAt verifies that CreatedAt is set and recent.
func TestGenerateCreatedAt(t *testing.T) {
	wf := makeWorkflow("wf-ts", dsl.BatchConfig{Strategy: "serial"})

	before := time.Now().Add(-time.Second)
	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	after := time.Now().Add(time.Second)

	assert.True(t, plan.CreatedAt.After(before), "CreatedAt should be after test start")
	assert.True(t, plan.CreatedAt.Before(after), "CreatedAt should be before test end")
}

// TestGenerateUnknownStrategy verifies that an unknown batch strategy
// produces an error.
func TestGenerateUnknownStrategy(t *testing.T) {
	wf := makeWorkflow("wf-unknown", dsl.BatchConfig{Strategy: "bogus"})

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.Error(t, err)
	require.Nil(t, plan)
	assert.Contains(t, err.Error(), "LE034")
}

// TestGenerateStepsSharedAcrossBatches verifies that every batch
// receives the same step sequence.
func TestGenerateStepsSharedAcrossBatches(t *testing.T) {
	steps := []dsl.Step{
		{Name: "s1", Module: "shell", Action: "exec"},
		{Name: "s2", Module: "pkg", Action: "install"},
	}
	wf := makeWorkflow("wf-shared", dsl.BatchConfig{
		Strategy: "percent",
		Steps:    []int{10, 100},
	}, steps...)

	g := NewGenerator()
	plan, err := g.Generate(wf, makeTargets(10))
	require.NoError(t, err)
	require.Len(t, plan.Batches, 2)

	for i, b := range plan.Batches {
		require.Len(t, b.Steps, 2, "batch %d should have 2 steps", i)
		assert.Equal(t, "s1", b.Steps[0].Name)
		assert.Equal(t, "s2", b.Steps[1].Name)
	}
}

// TestGeneratePercentTwoSteps verifies a percent strategy with exactly
// two milestones [50, 100].
func TestGeneratePercentTwoSteps(t *testing.T) {
	wf := makeWorkflow("wf-two", dsl.BatchConfig{
		Strategy: "percent",
		Steps:    []int{50, 100},
	})

	g := NewGenerator()
	plan, err := g.Generate(wf, makeTargets(10))
	require.NoError(t, err)
	require.Len(t, plan.Batches, 2)
	assert.Len(t, plan.Batches[0].Targets, 5)
	assert.Len(t, plan.Batches[1].Targets, 5)
}

// TestGenerateFixedSingleStep verifies a fixed strategy with a single
// step value that is larger than the target count.
func TestGenerateFixedSingleStep(t *testing.T) {
	wf := makeWorkflow("wf-fixed-one", dsl.BatchConfig{
		Strategy: "fixed",
		Steps:    []int{100},
	})

	g := NewGenerator()
	plan, err := g.Generate(wf, makeTargets(5))
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	assert.Len(t, plan.Batches[0].Targets, 5)
}

// TestGenerateEmptySteps verifies that a workflow with no steps
// produces batches with empty step slices (but the plan still has
// batches).
func TestGenerateEmptySteps(t *testing.T) {
	wf := makeWorkflow("wf-no-steps", dsl.BatchConfig{Strategy: "serial"})

	g := NewGenerator()
	plan, err := g.Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.Len(t, plan.Batches, 1)
	assert.Len(t, plan.Batches[0].Steps, 0)
}
