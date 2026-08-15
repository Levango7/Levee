package batch

import (
	"context"
	"fmt"
	"testing"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/plan"
)

// --- Controller benchmarks ---

func BenchmarkNewController(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewController()
	}
}

func BenchmarkController_Execute_Serial(b *testing.B) {
	b.ReportAllocs()
	ctrl := NewController()
	p := makeBenchPlan(3, 10, 2) // 3 batches, 10 targets, 2 steps
	execFn := func(ctx context.Context, b plan.Batch, target string, step plan.PlanStep) error {
		return nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ctrl.Execute(ctx, p, execFn)
	}
}

func BenchmarkController_Execute_Parallel(b *testing.B) {
	b.ReportAllocs()
	ctrl := NewController(WithTargetErrorPolicy(PolicyContinue))
	p := makeBenchPlan(5, 50, 3) // 5 batches, 50 targets, 3 steps
	execFn := func(ctx context.Context, b plan.Batch, target string, step plan.PlanStep) error {
		return nil
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ctrl.Execute(ctx, p, execFn)
	}
}

// --- ConcurrencyManager benchmarks ---

func BenchmarkConcurrencyManager_AcquireRelease_Unlimited(b *testing.B) {
	b.ReportAllocs()
	mgr := NewConcurrencyManager(nil, "batch", 0)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release, _ := mgr.Acquire(ctx, "host1")
		release()
	}
}

func BenchmarkConcurrencyManager_AcquireRelease_WithLimiter(b *testing.B) {
	b.ReportAllocs()
	limiter := channel.NewLimiter(1000, 100, 10, 0)
	mgr := NewConcurrencyManager(limiter, "batch", 0)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release, _ := mgr.Acquire(ctx, "host1")
		release()
	}
}

func BenchmarkConcurrencyManager_Stats(b *testing.B) {
	b.ReportAllocs()
	mgr := NewConcurrencyManager(nil, "batch", 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mgr.Stats()
	}
}

// --- ErrorPolicy benchmarks ---

func BenchmarkErrorPolicy_String(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = PolicyContinue.String()
		_ = PolicyAbort.String()
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
