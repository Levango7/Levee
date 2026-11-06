package state

import (
	"context"
	"testing"
	"time"
)

// G-2: the store reports "now" from its own clock so a caller that compares
// against backend-stamped timestamps uses the same clock that wrote them.

func TestSQLiteStoreNow(t *testing.T) {
	ctx := context.Background()
	s, err := NewSQLiteStore(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	before := time.Now().UTC()
	got, err := s.StoreNow(ctx)
	if err != nil {
		t.Fatalf("StoreNow: %v", err)
	}
	after := time.Now().UTC()
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("StoreNow = %v, outside [%v, %v]", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("StoreNow location = %v, want UTC", got.Location())
	}
}

func TestPGStoreNow(t *testing.T) {
	ctx := context.Background()
	s, cleanup := newPGTestStore(t)
	defer cleanup()

	got, err := s.StoreNow(ctx)
	if err != nil {
		t.Fatalf("StoreNow: %v", err)
	}
	// The PostgreSQL server clock is normally within a small margin; assert a
	// generous sanity window rather than equality with the process clock —
	// the whole point is that they may differ.
	if d := time.Since(got); d > time.Hour || d < -time.Hour {
		t.Fatalf("PG StoreNow = %v is %v from the process clock; implausible", got, d)
	}
	if got.Location() != time.UTC {
		t.Fatalf("PG StoreNow location = %v, want UTC", got.Location())
	}
}
