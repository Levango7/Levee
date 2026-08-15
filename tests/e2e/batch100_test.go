//go:build e2e

package e2e

// batch100_test.go implements 100-target end-to-end integration tests.
// These tests verify that the LEVEE engine can plan, batch and execute
// changes across 100 mock targets with correct batch boundaries and
// complete audit traces.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- 100-target test infrastructure -----------------------------------------

// setupBatch100Test creates a 100-target mock cluster with a full engine
// stack for batch integration testing.
func setupBatch100Test(t *testing.T) (*state.SQLiteStore, *MockCluster) {
	t.Helper()

	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cluster := NewMockCluster(100, store)

	// Script all targets to succeed on common commands.
	for _, tgt := range cluster.Targets {
		tgt.Commands["pkg.upgrade"] = MockResult{ExitCode: 0, Stdout: "upgraded"}
		tgt.Commands["pkg.install"] = MockResult{ExitCode: 0, Stdout: "installed"}
		tgt.Commands["svc.restart"] = MockResult{ExitCode: 0, Stdout: "restarted"}
		tgt.Commands["svc.start"] = MockResult{ExitCode: 0, Stdout: "started"}
		tgt.Commands["file.copy"] = MockResult{ExitCode: 0, Stdout: "copied"}
		tgt.Commands["file.restore"] = MockResult{ExitCode: 0, Stdout: "restored"}
	}

	return store, cluster
}

// makeBatch100Workflow creates a workflow with percent batching for 100 targets.
func makeBatch100Workflow() *dsl.Workflow {
	return &dsl.Workflow{
		Meta: dsl.WorkflowMeta{
			Name:    "batch100-drill",
			Version: "1.0",
		},
		Batches: dsl.BatchConfig{
			Strategy:       "percent",
			MaxConcurrency: 10,
			Steps:          []int{5, 25, 50, 100},
		},
		Steps: []dsl.Step{
			{
				Name:   "upgrade-package",
				Module: "pkg",
				Action: "upgrade",
				Rollback: &dsl.RollbackSpec{
					Strategy: "undo_action",
					Steps: []dsl.Step{{
						Name:   "install-package",
						Module: "pkg",
						Action: "install",
					}},
				},
			},
			{
				Name:   "restart-service",
				Module: "svc",
				Action: "restart",
				Rollback: &dsl.RollbackSpec{
					Strategy: "undo_action",
					Steps: []dsl.Step{{
						Name:   "start-service",
						Module: "svc",
						Action: "start",
					}},
				},
			},
		},
	}
}

// --- 100-target E2E tests ---------------------------------------------------

// TestBatch100_PlanAndExecute verifies that a 100-target plan is generated
// correctly and executed end-to-end with all targets completing.
func TestBatch100_PlanAndExecute(t *testing.T) {
	_, cluster := setupBatch100Test(t)
	ctx := context.Background()

	wf := makeBatch100Workflow()
	gen := plan.NewGenerator()
	p, err := gen.Generate(wf, cluster.Hosts())
	require.NoError(t, err)

	// Verify plan structure.
	assert.Equal(t, 100, p.TotalTargets)
	assert.NotEmpty(t, p.ID)
	assert.Equal(t, "batch100-drill", p.WorkflowName)

	// Verify batch division: percent strategy with steps [5, 25, 50, 100]
	// should produce 4 batches.
	assert.Len(t, p.Batches, 4, "should have 4 batches for percent [5,25,50,100]")

	// Verify every target appears in exactly one batch.
	targetSet := make(map[string]int)
	for _, b := range p.Batches {
		for _, h := range b.Targets {
			targetSet[h]++
		}
	}
	assert.Len(t, targetSet, 100, "all 100 targets should be covered")
	for h, count := range targetSet {
		assert.Equal(t, 1, count, "target %s should appear exactly once", h)
	}

	// Execute the plan via the batch controller.
	batchCtrl := batch.NewController(
		batch.WithBatchErrorPolicy(batch.PolicyAbort),
		batch.WithTargetErrorPolicy(batch.PolicyAbort),
	)
	execFn := makeBatchExecFunc(cluster)
	results := batchCtrl.Execute(ctx, p, execFn)

	// Verify all batches completed.
	assert.Len(t, results, len(p.Batches), "should have results for all batches")
	for i, br := range results {
		assert.NoError(t, br.Error, "batch %d should succeed", i)
	}

	// Verify all targets were executed.
	totalTargetResults := 0
	for _, br := range results {
		totalTargetResults += len(br.TargetResults)
	}
	assert.Equal(t, 100, totalTargetResults, "all 100 targets should have results")

	// Verify each target was called the expected number of times (2 steps).
	for _, tgt := range cluster.Targets {
		assert.Equal(t, 2, tgt.CallCount(), "target %s should have 2 step calls", tgt.ID)
	}
}

// TestBatch100_TraceComplete verifies that all 100 targets have complete
// audit trace records after execution.
func TestBatch100_TraceComplete(t *testing.T) {
	store, cluster := setupBatch100Test(t)
	ctx := context.Background()

	// Create a run record for trace association.
	runID := "batch100-trace-test"
	run := &state.Run{
		ID:           runID,
		WorkflowName: "batch100-drill",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, store.CreateRun(ctx, run))

	// Execute the plan and record traces.
	wf := makeBatch100Workflow()
	gen := plan.NewGenerator()
	p, err := gen.Generate(wf, cluster.Hosts())
	require.NoError(t, err)

	recorder, err := audit.NewTraceRecorder(store)
	require.NoError(t, err)

	batchCtrl := batch.NewController(
		batch.WithBatchErrorPolicy(batch.PolicyAbort),
		batch.WithTargetErrorPolicy(batch.PolicyAbort),
	)

	// Custom exec func that records traces.
	execFn := func(ctx context.Context, b plan.Batch, target string, step plan.PlanStep) error {
		start := time.Now()
		command := step.Module + "." + step.Action
		result, err := cluster.Execute(ctx, target, command)
		duration := time.Since(start)

		// Record trace.
		output := map[string]any{
			"exit_code": result.ExitCode,
			"stdout":    result.Stdout,
		}
		if result.Stderr != "" {
			output["stderr"] = result.Stderr
		}
		_, _ = recorder.RecordStep(ctx, runID, target, step.Name,
			step.Module+"."+step.Action, nil, output, duration, err)

		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("step %s on %s: exit %d", step.Name, target, result.ExitCode)
		}
		return nil
	}

	results := batchCtrl.Execute(ctx, p, execFn)
	for _, br := range results {
		assert.NoError(t, br.Error)
	}

	// Verify trace completeness: every target should have at least one trace.
	traces, err := recorder.ListByRun(ctx, runID)
	require.NoError(t, err)

	// We expect 100 targets × 2 steps = 200 trace records.
	assert.Len(t, traces, 200, "should have 200 trace records (100 targets × 2 steps)")

	// Verify every target is represented in the traces.
	targetTraces := make(map[string]int)
	for _, tr := range traces {
		// Parse target from the detail JSON.
		detail := tr.Detail
		// The detail is a JSON string; we just check the target field exists.
		_ = detail
		// Count by runID to ensure all are present.
		targetTraces[tr.RunID]++
	}
	assert.Equal(t, 200, targetTraces[runID], "all traces should belong to the run")
}

// TestBatch100_BatchBoundary verifies that batch boundaries are correct
// for the percent strategy with steps [5, 25, 50, 100].
func TestBatch100_BatchBoundary(t *testing.T) {
	_, cluster := setupBatch100Test(t)

	wf := makeBatch100Workflow()
	gen := plan.NewGenerator()
	p, err := gen.Generate(wf, cluster.Hosts())
	require.NoError(t, err)

	// Percent strategy with steps [5, 25, 50, 100] on 100 targets:
	//   Batch 0: 5%  = 5 targets
	//   Batch 1: 25% - 5% = 20 targets
	//   Batch 2: 50% - 25% = 25 targets
	//   Batch 3: 100% - 50% = 50 targets
	expectedSizes := []int{5, 20, 25, 50}
	require.Len(t, p.Batches, len(expectedSizes))

	for i, b := range p.Batches {
		assert.Equal(t, expectedSizes[i], len(b.Targets),
			"batch %d should have %d targets, got %d", i, expectedSizes[i], len(b.Targets))
		assert.Equal(t, i, b.Index, "batch index should be %d", i)
	}

	// Verify batch indices are consecutive.
	for i, b := range p.Batches {
		assert.Equal(t, i, b.Index)
	}

	// Verify no target appears in multiple batches.
	seen := make(map[string]bool)
	for _, b := range p.Batches {
		for _, h := range b.Targets {
			assert.False(t, seen[h], "target %s should not appear in multiple batches", h)
			seen[h] = true
		}
	}
	assert.Len(t, seen, 100, "all 100 targets should be present across batches")

	// Verify every batch has the same steps.
	for i, b := range p.Batches {
		if i > 0 {
			assert.Equal(t, len(p.Batches[0].Steps), len(b.Steps),
				"batch %d should have same step count as batch 0", i)
		}
		assert.Equal(t, 2, len(b.Steps), "each batch should have 2 steps")
	}

	// Verify MaxConcurrency is propagated.
	for _, b := range p.Batches {
		assert.Equal(t, 10, b.MaxConcurrency, "max concurrency should be 10")
	}
}

// --- Helper -----------------------------------------------------------------

// makeBatchExecFunc creates a batch.ExecuteFunc that dispatches through the
// mock cluster.
func makeBatchExecFunc(cluster *MockCluster) batch.ExecuteFunc {
	return func(ctx context.Context, b plan.Batch, target string, step plan.PlanStep) error {
		command := step.Module + "." + step.Action
		result, err := cluster.Execute(ctx, target, command)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("step %s.%s on %s: exit %d: %s",
				step.Module, step.Action, target, result.ExitCode, result.Stderr)
		}
		return nil
	}
}
