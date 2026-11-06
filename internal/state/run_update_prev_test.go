package state

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// G-5: RunStatusPrevReader.UpdateRunReturningPrevStatus must overwrite the row
// exactly like UpdateRun AND report the status the write superseded, atomically.

func TestUpdateRunReturningPrevStatus_SQLite(t *testing.T) {
	ctx := context.Background()
	s, err := NewSQLiteStore(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	run := newTestRun("chg-prev", time.Now().UTC())
	run.Status = "running"
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	run.Status = "paused"
	run.UpdatedAt = time.Now().UTC()
	prev, err := s.UpdateRunReturningPrevStatus(ctx, run)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if prev != "running" {
		t.Fatalf("prev = %q, want running", prev)
	}

	// Same write semantics as UpdateRun: the row now carries the new value.
	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "paused" {
		t.Fatalf("stored status = %q, want paused", got.Status)
	}

	// A second update reports the status the first one left behind.
	run.Status = "archived"
	prev, err = s.UpdateRunReturningPrevStatus(ctx, run)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if prev != "paused" {
		t.Fatalf("second prev = %q, want paused", prev)
	}
}

func TestUpdateRunReturningPrevStatus_SQLiteNotFound(t *testing.T) {
	ctx := context.Background()
	s, err := NewSQLiteStore(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	_, err = s.UpdateRunReturningPrevStatus(ctx, &Run{ID: "chg-ghost", Status: "running", UpdatedAt: time.Now().UTC()})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error like UpdateRun, got %v", err)
	}
}

func TestUpdateRunReturningPrevStatus_NilRun(t *testing.T) {
	ctx := context.Background()
	s, _ := NewSQLiteStore(ctx, ":memory:")
	defer s.Close()
	if _, err := s.UpdateRunReturningPrevStatus(ctx, nil); err == nil {
		t.Fatal("nil run must be rejected")
	}
}

// TestUpdateRunReturningPrevStatus_SQLiteConcurrent pins the atomicity: with N
// goroutines doing a read-modify-write on the same row, the statuses they
// report as superseded must form one unbroken chain — each is a status another
// goroutine wrote, none repeats, and none is lost. This is exactly the property
// the read-then-write fallback cannot provide.
func TestUpdateRunReturningPrevStatus_SQLiteConcurrent(t *testing.T) {
	ctx := context.Background()
	// A file (not :memory:) so the store keeps a real pool and the writers
	// contend on the database lock as they would in production; :memory: is
	// pinned to a single connection.
	s, err := NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "prev.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	run := newTestRun("chg-race", time.Now().UTC())
	run.Status = "start"
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 24
	written := make([]string, n)
	for i := range written {
		written[i] = fmt.Sprintf("s%02d", i)
	}

	prevs := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			row, err := s.GetRun(ctx, run.ID)
			if err != nil {
				errs[i] = err
				return
			}
			row.Status = written[i]
			row.UpdatedAt = time.Now().UTC()
			prevs[i], errs[i] = s.UpdateRunReturningPrevStatus(ctx, row)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}

	final, err := s.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}

	// The chain: {reported-prevs} ∪ {final} == {start} ∪ {written}, each once.
	seen := map[string]int{}
	for _, p := range prevs {
		seen[p]++
	}
	seen[final.Status]++
	for _, w := range written {
		if seen[w] != 1 {
			t.Fatalf("status %q appears %d times as a superseded/final value, want exactly 1 (lost or duplicated write)", w, seen[w])
		}
	}
	if seen["start"] != 1 {
		t.Fatalf("the initial status must be superseded exactly once, saw %d", seen["start"])
	}
	if len(seen) != n+1 {
		t.Fatalf("expected %d distinct chain links, got %d: %v", n+1, len(seen), seen)
	}
}

// PostgreSQL path of the same contract. Skipped unless LEVEE_PG_TEST_DSN is
// set; the row lock (SELECT ... FOR UPDATE) is the part that must hold across
// cluster members, which the SQLite test cannot exercise.
func TestUpdateRunReturningPrevStatus_Postgres(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newPGTestStore(t)
	defer cleanup()

	run := newTestRun("chg-pg-prev", time.Now().UTC())
	run.Status = "running"
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("create: %v", err)
	}

	run.Status = "paused"
	run.UpdatedAt = time.Now().UTC()
	prev, err := s.UpdateRunReturningPrevStatus(ctx, run)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if prev != "running" {
		t.Fatalf("prev = %q, want running", prev)
	}

	got, err := s.GetRun(ctx, run.ID)
	if err != nil || got == nil || got.Status != "paused" {
		t.Fatalf("stored row after update: %v status=%v", err, got)
	}

	if _, err := s.UpdateRunReturningPrevStatus(ctx, &Run{ID: "chg-pg-ghost", Status: "running", UpdatedAt: time.Now().UTC()}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found for a missing row, got %v", err)
	}
}
