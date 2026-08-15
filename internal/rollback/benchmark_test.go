package rollback

import (
	"context"
	"fmt"
	"testing"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
)

// --- Manager benchmarks ---

func BenchmarkNewManager(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewManager()
	}
}

func BenchmarkManager_Rollback_Serial(b *testing.B) {
	b.ReportAllocs()
	m := NewManager()
	p := makeBenchPlan(3, 10, 2)
	execFn := func(ctx context.Context, target string, step dsl.Step) error {
		return nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Rollback(ctx, p, execFn)
	}
}

func BenchmarkManager_Rollback_Parallel(b *testing.B) {
	b.ReportAllocs()
	m := NewManager(WithConcurrency(5))
	p := makeBenchPlan(5, 50, 3)
	execFn := func(ctx context.Context, target string, step dsl.Step) error {
		return nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Rollback(ctx, p, execFn)
	}
}

func BenchmarkManager_Rollback_WithWhitelist(b *testing.B) {
	b.ReportAllocs()
	m := NewManager(WithWhitelist([]string{"pkg.install", "file.copy"}))
	p := makeBenchPlanWithRollback(3, 10, 2)
	execFn := func(ctx context.Context, target string, step dsl.Step) error {
		return nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Rollback(ctx, p, execFn)
	}
}

func BenchmarkManager_Whitelist(b *testing.B) {
	b.ReportAllocs()
	m := NewManager(WithWhitelist([]string{"pkg.install", "file.copy", "svc.restart"}))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Whitelist()
	}
}

func BenchmarkIsSkipped(b *testing.B) {
	b.ReportAllocs()
	sr := StepRollbackResult{Skipped: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = IsSkipped(sr)
	}
}

func BenchmarkHasError(b *testing.B) {
	b.ReportAllocs()
	sr := StepRollbackResult{Skipped: false, Error: nil}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = HasError(sr)
	}
}

// --- helpers ---

func makeBenchPlan(batchCount, targetsPerBatch, stepsPerBatch int) *plan.Plan {
	batches := make([]plan.Batch, batchCount)
	for i := 0; i < batchCount; i++ {
		targets := make([]string, targetsPerBatch)
		for j := 0; j < targetsPerBatch; j++ {
			targets[j] = fmt.Sprintf("host-%d-%d", i, j)
		}
		steps := make([]plan.PlanStep, stepsPerBatch)
		for k := 0; k < stepsPerBatch; k++ {
			steps[k] = plan.PlanStep{
				Name:   fmt.Sprintf("step-%d", k),
				Module: "shell",
				Action: "exec",
				Args:   map[string]any{"cmd": "echo ok"},
			}
		}
		batches[i] = plan.Batch{
			Index:          i,
			Targets:        targets,
			Steps:          steps,
			MaxConcurrency: 10,
		}
	}
	return &plan.Plan{
		ID:           "bench-plan",
		WorkflowName: "bench-workflow",
		Batches:      batches,
		TotalTargets: batchCount * targetsPerBatch,
	}
}

func makeBenchPlanWithRollback(batchCount, targetsPerBatch, stepsPerBatch int) *plan.Plan {
	batches := make([]plan.Batch, batchCount)
	for i := 0; i < batchCount; i++ {
		targets := make([]string, targetsPerBatch)
		for j := 0; j < targetsPerBatch; j++ {
			targets[j] = fmt.Sprintf("host-%d-%d", i, j)
		}
		steps := make([]plan.PlanStep, stepsPerBatch)
		for k := 0; k < stepsPerBatch; k++ {
			steps[k] = plan.PlanStep{
				Name:   fmt.Sprintf("step-%d", k),
				Module: "pkg",
				Action: "remove",
				Args:   map[string]any{"name": "nginx"},
				Rollback: &dsl.RollbackSpec{
					Steps: []dsl.Step{
						{
							Name:   fmt.Sprintf("undo-step-%d", k),
							Module: "pkg",
							Action: "install",
							Args:   map[string]any{"name": "nginx"},
						},
					},
				},
			}
		}
		batches[i] = plan.Batch{
			Index:          i,
			Targets:        targets,
			Steps:          steps,
			MaxConcurrency: 10,
		}
	}
	return &plan.Plan{
		ID:           "bench-plan-rb",
		WorkflowName: "bench-workflow-rb",
		Batches:      batches,
		TotalTargets: batchCount * targetsPerBatch,
	}
}
