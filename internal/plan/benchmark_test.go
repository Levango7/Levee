package plan

import (
	"fmt"
	"testing"

	"github.com/nexus/levee/internal/dsl"
)

// --- Generator benchmarks ---

func BenchmarkNewGenerator(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewGenerator()
	}
}

func BenchmarkGenerator_Generate_Serial(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("serial", nil)
	targets := makeBenchTargets(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = g.Generate(wf, targets)
	}
}

func BenchmarkGenerator_Generate_Percent(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = g.Generate(wf, targets)
	}
}

func BenchmarkGenerator_Generate_Fixed(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("fixed", []int{5, 10, 20})
	targets := makeBenchTargets(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = g.Generate(wf, targets)
	}
}

func BenchmarkGenerator_Generate_Large(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = g.Generate(wf, targets)
	}
}

// --- Hash benchmarks ---

func BenchmarkComputeHash(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(100)
	p, _ := g.Generate(wf, targets)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ComputeHash(p)
	}
}

func BenchmarkComputeHash_Large(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(1000)
	p, _ := g.Generate(wf, targets)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ComputeHash(p)
	}
}

func BenchmarkVerifyHash(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(100)
	p, _ := g.Generate(wf, targets)
	expected := ComputeHash(p)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = VerifyHash(p, expected)
	}
}

func BenchmarkBuildCanonical(b *testing.B) {
	b.ReportAllocs()
	g := NewGenerator()
	wf := makeBenchWorkflow("percent", []int{1, 10, 50, 100})
	targets := makeBenchTargets(100)
	p, _ := g.Generate(wf, targets)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildCanonical(p)
	}
}

func BenchmarkSortedCopy(b *testing.B) {
	b.ReportAllocs()
	in := make([]string, 100)
	for i := range in {
		in[i] = fmt.Sprintf("host-%03d", 99-i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sortedCopy(in)
	}
}

func BenchmarkNewPlanID(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = newPlanID()
	}
}

// --- helpers ---

func makeBenchWorkflow(strategy string, steps []int) *dsl.Workflow {
	return &dsl.Workflow{
		Meta: dsl.WorkflowMeta{Name: "bench-workflow", Version: "1.0"},
		Targets: []dsl.TargetGroup{
			{Name: "web", Hosts: []string{"host1"}, Type: "ssh"},
		},
		Batches: dsl.BatchConfig{
			Strategy:       strategy,
			Steps:          steps,
			MaxConcurrency: 10,
		},
		Steps: []dsl.Step{
			{Name: "step-1", Module: "shell", Action: "exec", Args: map[string]any{"cmd": "echo ok"}},
			{Name: "step-2", Module: "pkg", Action: "install", Args: map[string]any{"name": "nginx"}},
		},
	}
}

func makeBenchTargets(count int) []string {
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = fmt.Sprintf("host-%d.example.com", i)
	}
	return out
}
