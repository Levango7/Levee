package audit

// trace_chain_order_test.go — the ordering invariant a per-run hash chain lives
// or dies by. Record seals after every insert, so the walk order
// (timestamp, id) must never place a new record in front of an already sealed
// one: that moves the older record off the predecessor its digest was computed
// against, and the next seal reports the run as tampered. The tie-break is the
// id, which is why ids have to sort in creation order (see newID).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// TestNewIDSortsInCreationOrder pins the property the chain order depends on:
// consecutive ids strictly increase. A loop with no I/O between iterations is
// exactly where a clock with sub-microsecond granularity reports the same value
// twice, so the strictness comes from the counter, not from the clock.
func TestNewIDSortsInCreationOrder(t *testing.T) {
	const n = 2000
	ids := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := newID()
		require.NoError(t, err)
		require.Len(t, id, 32)
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q at index %d", id, i)
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		assert.Greater(t, ids[i], ids[i-1], "id %d (%s) must sort after id %d (%s)", i, ids[i], i-1, ids[i-1])
	}
}

// TestSeal_AppendNeverRepositionsSealedRow reproduces the production sequence —
// insert, seal, insert, seal — with every record sharing one timestamp, so the
// id is the only thing deciding the order. Records issued by newID therefore
// walk in creation order and each seal only ever extends the tail.
//
// The timestamps are pinned to one value rather than left to the clock so the
// assertion holds on every platform: a nanosecond-granularity clock (CI's Linux
// runners) would otherwise never tie, and the test would pass without exercising
// the tie-break at all.
func TestSeal_AppendNeverRepositionsSealedRow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createRun(t, store, "run-tie")

	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)

	sameTS := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	appended := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		id, err := newID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTrace(ctx, &state.Trace{
			ID:        id,
			RunID:     "run-tie",
			Event:     EventStepExecute,
			Actor:     "system",
			Detail:    fmt.Sprintf(`{"index":%d}`, i),
			Timestamp: sameTS,
		}))
		// A seal that repositions an already sealed record returns
		// ErrChainBroken instead of rewriting it, so this line is the assertion.
		res, err := b.Seal(ctx, "run-tie")
		require.NoError(t, err, "seal %d must extend the chain, not report a break", i)
		assert.Equal(t, 1, res.Sealed, "seal %d must write exactly the record just appended", i)
		appended = append(appended, id)
	}

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tie"})
	require.NoError(t, err)
	walked := make([]string, 0, len(traces))
	for _, tr := range traces {
		walked = append(walked, tr.ID)
	}
	assert.Equal(t, appended, walked, "the chain walk order must be the append order")

	_, err = b.Verify(ctx, "run-tie")
	assert.NoError(t, err, "a chain sealed on append must verify")
}

// TestSeal_ProductionPathStaysVerifiableAcrossManyAppends is the same invariant
// reached through TraceRecorder.Record, at the volume where the real clock does
// tie locally.
func TestSeal_ProductionPathStaysVerifiableAcrossManyAppends(t *testing.T) {
	store := newTestStore(t)
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)
	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	ctx := context.Background()
	createRun(t, store, "run-loop")

	// Record seals internally and only logs a failed seal, so the seal here is
	// what surfaces the damage: a repositioned record makes it return
	// ErrChainBroken.
	for i := 0; i < 60; i++ {
		_, err := rec.Record(ctx, TraceRecord{
			RunID: "run-loop", Event: EventStepExecute, Actor: "system",
			Target: "host-1", Input: map[string]any{"p": i}, Output: map[string]any{"ok": true},
		})
		require.NoError(t, err)
		_, err = b.Seal(ctx, "run-loop")
		require.NoError(t, err, "seal after append %d", i)
	}

	_, err = b.Verify(ctx, "run-loop")
	require.NoError(t, err)
}
