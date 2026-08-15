package lock

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
)

// --- Lock struct benchmarks ---

func BenchmarkLock_Expired(b *testing.B) {
	b.ReportAllocs()
	now := time.Now().UTC()
	l := &Lock{
		Target:     "host1",
		Owner:      "run-001",
		AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt:  now.Add(-1 * time.Hour),
		TTL:        1 * time.Hour,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = l.Expired(now)
	}
}

func BenchmarkLock_Expired_NotExpired(b *testing.B) {
	b.ReportAllocs()
	now := time.Now().UTC()
	l := &Lock{
		Target:     "host1",
		Owner:      "run-001",
		AcquiredAt: now,
		ExpiresAt:  now.Add(1 * time.Hour),
		TTL:        1 * time.Hour,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = l.Expired(now)
	}
}

func BenchmarkScope(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scope("host1")
	}
}

// --- SQLite-backed LockStore benchmarks ---

func BenchmarkLockStore_Acquire(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_lock.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ls := NewLockStore(store)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target := fmt.Sprintf("host-%d", i)
		_, _ = ls.Acquire(ctx, target, "run-001", DefaultTTL)
	}
}

func BenchmarkLockStore_Release(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_lock_release.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ls := NewLockStore(store)
	// Pre-acquire locks
	for i := 0; i < 1000; i++ {
		_, _ = ls.Acquire(ctx, fmt.Sprintf("host-%d", i), "run-001", DefaultTTL)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ls.Release(ctx, fmt.Sprintf("host-%d", i%1000), "run-001")
	}
}

func BenchmarkLockStore_Get(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_lock_get.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ls := NewLockStore(store)
	_, _ = ls.Acquire(ctx, "host1", "run-001", DefaultTTL)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ls.Get(ctx, "host1")
	}
}

func BenchmarkLockManager_Acquire(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_lockmgr.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ls := NewLockStore(store)
	mgr := NewLockManager(ls, store)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target := fmt.Sprintf("host-%d", i)
		_, _ = mgr.Acquire(ctx, target, "run-001")
	}
}

func BenchmarkLockManager_Release(b *testing.B) {
	b.ReportAllocs()
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "bench_lockmgr_release.db")
	store, err := state.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	ls := NewLockStore(store)
	mgr := NewLockManager(ls, store)
	// Pre-acquire
	for i := 0; i < 1000; i++ {
		_, _ = mgr.Acquire(ctx, fmt.Sprintf("host-%d", i), "run-001")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = mgr.Release(ctx, fmt.Sprintf("host-%d", i%1000), "run-001")
	}
}
