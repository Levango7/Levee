package approval

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// --- LevelManager benchmarks ---

func BenchmarkNewLevelManager(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewLevelManager()
	}
}

func BenchmarkLevelManager_DetermineLevel_Standard(b *testing.B) {
	b.ReportAllocs()
	m := NewLevelManager()
	step := Step{Module: "pkg", Action: "install", Irreversible: false, Emergency: false}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.DetermineLevel(step)
	}
}

func BenchmarkLevelManager_DetermineLevel_High(b *testing.B) {
	b.ReportAllocs()
	m := NewLevelManager()
	step := Step{Module: "pkg", Action: "remove", Irreversible: true, Emergency: false}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.DetermineLevel(step)
	}
}

func BenchmarkLevelManager_DetermineLevel_Emergency(b *testing.B) {
	b.ReportAllocs()
	m := NewLevelManager()
	step := Step{Module: "svc", Action: "restart", Irreversible: true, Emergency: true}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.DetermineLevel(step)
	}
}

func BenchmarkLevelManager_Get(b *testing.B) {
	b.ReportAllocs()
	m := NewLevelManager()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Get(LevelStandard)
	}
}

func BenchmarkLevelManager_All(b *testing.B) {
	b.ReportAllocs()
	m := NewLevelManager()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.All()
	}
}

// --- Service benchmarks (in-memory) ---

func BenchmarkService_Create(b *testing.B) {
	b.ReportAllocs()
	store := newMockStore()
	svc := NewService(store)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = svc.Create(ctx, CreateRequest{
			RunID:        fmt.Sprintf("run-%d", i),
			Level:        LevelStandard,
			Approvers:    []string{"alice", "bob"},
			MinApprovers: 1,
			ExpiresAt:    expiresAt,
		})
	}
}

func BenchmarkService_Approve(b *testing.B) {
	b.ReportAllocs()
	store := newMockStore()
	svc := NewService(store)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	a, _ := svc.Create(ctx, CreateRequest{
		RunID:        "run-bench",
		Level:        LevelStandard,
		Approvers:    []string{"alice", "bob"},
		MinApprovers: 1,
		ExpiresAt:    expiresAt,
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = svc.Approve(ctx, a.ID, "alice")
	}
}

func BenchmarkService_Reject(b *testing.B) {
	b.ReportAllocs()
	store := newMockStore()
	svc := NewService(store)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a, _ := svc.Create(ctx, CreateRequest{
			RunID:        fmt.Sprintf("run-%d", i),
			Level:        LevelStandard,
			Approvers:    []string{"alice"},
			MinApprovers: 1,
			ExpiresAt:    expiresAt,
		})
		_ = svc.Reject(ctx, a.ID, "alice", "not approved")
	}
}

func BenchmarkService_Get(b *testing.B) {
	b.ReportAllocs()
	store := newMockStore()
	svc := NewService(store)
	ctx := context.Background()
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	a, _ := svc.Create(ctx, CreateRequest{
		RunID:        "run-bench",
		Level:        LevelStandard,
		Approvers:    []string{"alice"},
		MinApprovers: 1,
		ExpiresAt:    expiresAt,
	})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = svc.Get(ctx, a.ID)
	}
}

func BenchmarkCanTransition(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = canTransition(StatusPending, StatusApproved)
	}
}

func BenchmarkValidLevel(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = validLevel("standard")
	}
}

func BenchmarkNewID(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = newID()
	}
}
