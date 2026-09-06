package state

// SA-014 (C-7): duplicate trace inserts must surface as the package sentinel
// ErrTraceExists on both backends, detected via typed driver errors rather
// than error-string matching.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRunForTrace(t *testing.T, store Store, id string, now time.Time) {
	t.Helper()
	require.NoError(t, store.CreateRun(context.Background(), &Run{
		ID: id, WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
}

func dupTrace(id, runID string, now time.Time) *Trace {
	return &Trace{
		ID: id, RunID: runID, Event: "step_end", Actor: "executor",
		Detail: `{}`, CurrHash: "c", Timestamp: now,
	}
}

// writeOnceDoubleWrite races two CreateTrace calls for the same id on any
// Store and asserts exactly one wins while the loser receives an error
// matching ErrTraceExists (never a raw driver error).
func writeOnceDoubleWrite(t *testing.T, store Store, backend string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedRunForTrace(t, store, "run-dupe", now)

	var (
		wg       sync.WaitGroup
		okCount  atomic.Int32
		dupCount atomic.Int32
		otherErr atomic.Value
	)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := store.CreateTrace(ctx, dupTrace("trace-dupe", "run-dupe", now))
			switch {
			case err == nil:
				okCount.Add(1)
			case errors.Is(err, ErrTraceExists):
				dupCount.Add(1)
			default:
				otherErr.Store(err)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.False(t, otherErr.Load() != nil,
		"%s: loser must map to ErrTraceExists, got: %v", backend, otherErr.Load())
	assert.Equal(t, int32(1), okCount.Load(), "%s: exactly one insert must succeed", backend)
	assert.Equal(t, int32(1), dupCount.Load(), "%s: the other insert must report ErrTraceExists", backend)

	// The winning row is intact and exactly one row exists.
	traces, err := store.ListTraces(ctx, TraceFilter{RunID: "run-dupe"})
	require.NoError(t, err)
	require.Len(t, traces, 1)
}

func TestCreateTrace_DuplicateID_ErrTraceExists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	seedRunForTrace(t, store, "r1", now)

	require.NoError(t, store.CreateTrace(ctx, dupTrace("t1", "r1", now)))

	err := store.CreateTrace(ctx, dupTrace("t1", "r1", now.Add(time.Second)))
	require.Error(t, err)
	require.ErrorIs(t, err, ErrTraceExists)
	assert.Contains(t, err.Error(), `"t1"`, "duplicate error must name the offending id")

	// Other constraint failures (FK) must NOT be misclassified as duplicates.
	err = store.CreateTrace(ctx, dupTrace("t-fk", "no-such-run", now))
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrTraceExists, "FK violation is not a duplicate")

	// Non-error sanity: isUniqueConstraint(nil) is false; the literal driver
	// string still classifies via the deliberate fallback; unrelated errors
	// do not.
	assert.False(t, isUniqueConstraint(nil))
	assert.True(t, isUniqueConstraint(errors.New("UNIQUE constraint failed: trace.id")))
	assert.False(t, isUniqueConstraint(errors.New("disk I/O timeout")))
}

func TestCreateTrace_ConcurrentSameID_SQLite(t *testing.T) {
	writeOnceDoubleWrite(t, newTestStore(t), "sqlite")
}

func TestPGStore_CreateTrace_DuplicateID_ErrTraceExists(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedRunForTrace(t, store, "r1", now)

	require.NoError(t, store.CreateTrace(ctx, dupTrace("t1", "r1", now)))
	err := store.CreateTrace(ctx, dupTrace("t1", "r1", now.Add(time.Second)))
	require.ErrorIs(t, err, ErrTraceExists)
}

func TestPGStore_CreateTrace_ConcurrentSameID(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	writeOnceDoubleWrite(t, store, "postgres")
}
