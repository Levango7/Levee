package audit

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
)

// --- TraceRecorder benchmarks ---

func BenchmarkRedact(b *testing.B) {
	b.ReportAllocs()
	input := map[string]any{
		"username": "admin",
		"password": "secret123",
		"token":    "abc-def",
		"host":     "web1",
		"nested": map[string]any{
			"key":      "value",
			"password": "nested-secret",
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Redact(input)
	}
}

func BenchmarkRedact_Large(b *testing.B) {
	b.ReportAllocs()
	input := make(map[string]any, 50)
	for i := 0; i < 50; i++ {
		input[fmt.Sprintf("field_%d", i)] = fmt.Sprintf("value_%d", i)
	}
	input["password"] = "secret"
	input["token"] = "abc"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Redact(input)
	}
}

func BenchmarkBuildDetail(b *testing.B) {
	b.ReportAllocs()
	record := TraceRecord{
		RunID:    "run-001",
		Event:    EventStepExecute,
		Actor:    "system",
		Target:   "host1",
		Input:    map[string]any{"cmd": "echo ok"},
		Output:   map[string]any{"exit_code": 0, "stdout": "ok"},
		Duration: 100 * time.Millisecond,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = buildDetail(record)
	}
}

func BenchmarkIsSensitive(b *testing.B) {
	b.ReportAllocs()
	keys := []string{"password", "username", "token", "host", "secret", "name"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, k := range keys {
			_ = isSensitive(k)
		}
	}
}

func BenchmarkNewID(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = newID()
	}
}

// --- HashChain benchmarks ---

func BenchmarkComputeHash(b *testing.B) {
	b.ReportAllocs()
	trace := &state.Trace{
		ID:        "trace-001",
		RunID:     "run-001",
		Event:     "step_execute",
		Actor:     "system",
		Detail:    `{"target":"host1","input":{"cmd":"echo ok"}}`,
		Timestamp: time.Now().UTC(),
	}
	prevHash := "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ComputeHash(trace, prevHash)
	}
}

func BenchmarkComputeHash_Chain(b *testing.B) {
	b.ReportAllocs()
	// Simulate building a chain of 100 hashes
	traces := make([]*state.Trace, 100)
	for i := range traces {
		traces[i] = &state.Trace{
			ID:        fmt.Sprintf("trace-%03d", i),
			RunID:     "run-001",
			Event:     "step_execute",
			Actor:     "system",
			Detail:    fmt.Sprintf(`{"target":"host1","step":"step-%d"}`, i),
			Timestamp: time.Now().UTC(),
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		prevHash := ""
		for _, t := range traces {
			prevHash = ComputeHash(t, prevHash)
		}
	}
}

// --- SQLite-backed benchmarks ---

func BenchmarkTraceRecorder_Record(b *testing.B) {
	b.ReportAllocs()
	dbPath := filepath.Join(b.TempDir(), "bench_audit.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	// Seed a run.
	now := time.Now().UTC()
	_ = store.CreateRun(context.Background(), &state.Run{
		ID: "run-bench", WorkflowName: "wf", TemplateName: "t",
		Status: "running", CreatedAt: now, UpdatedAt: now, Creator: "bench",
	})

	rec, err := NewTraceRecorder(store)
	if err != nil {
		b.Fatal(err)
	}

	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = rec.RecordStep(ctx, "run-bench", "host1", "step-1", "shell.exec",
			map[string]any{"cmd": "echo ok"},
			map[string]any{"exit_code": 0},
			100*time.Millisecond,
			nil,
		)
	}
}
