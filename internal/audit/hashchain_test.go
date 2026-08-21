package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// newChainBuilder builds a HashChainBuilder on top of a fresh temp-file store
// and returns the underlying store so the test can insert runs/traces.
func newChainBuilder(t *testing.T) (*HashChainBuilder, *state.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	return b, store
}

// recordTraces records n traces for the given run with strictly increasing
// timestamps and returns the persisted trace ids. The traces use distinct
// events/actors/details so their hashes are distinct.
func recordTraces(t *testing.T, store state.Store, runID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)

	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		trace, err := rec.Record(ctx, TraceRecord{
			RunID:  runID,
			Event:  EventStepExecute,
			Actor:  "system",
			Target: "host:node-1",
			Input:  map[string]any{"step": i},
			Output: map[string]any{"ok": true},
			Metadata: map[string]string{
				"index": string(rune('a' + i)),
			},
		})
		require.NoError(t, err)
		ids = append(ids, trace.ID)
		// Sleep so timestamps are strictly increasing (SQLite stores with
		// microsecond precision; 1ms is plenty).
		time.Sleep(2 * time.Millisecond)
	}
	return ids
}

func TestNewHashChainBuilder_NilStore(t *testing.T) {
	b, err := NewHashChainBuilder(nil)
	require.ErrorIs(t, err, ErrNilStore)
	assert.Nil(t, b)
}

func TestBuild_BasicChain(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-1")
	recordTraces(t, store, "run-1", 3)

	count, tail, err := b.Build(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.NotEmpty(t, tail)

	// Reload and verify the chain structure.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-1"})
	require.NoError(t, err)
	require.Len(t, traces, 3)

	// First trace: PrevHash empty, CurrHash non-empty.
	assert.Empty(t, traces[0].PrevHash)
	assert.NotEmpty(t, traces[0].CurrHash)

	// Each subsequent trace: PrevHash == previous CurrHash.
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash,
			"trace %d PrevHash should equal previous CurrHash", i)
		assert.NotEmpty(t, traces[i].CurrHash)
	}

	// Tail hash should equal the last trace's CurrHash.
	assert.Equal(t, traces[len(traces)-1].CurrHash, tail)
}

func TestBuild_SingleTrace(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-single")
	recordTraces(t, store, "run-single", 1)

	count, tail, err := b.Build(ctx, "run-single")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, tail)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-single"})
	require.NoError(t, err)
	require.Len(t, traces, 1)
	assert.Empty(t, traces[0].PrevHash)
	assert.Equal(t, tail, traces[0].CurrHash)
}

func TestBuild_EmptyRunID(t *testing.T) {
	b, _ := newChainBuilder(t)
	_, _, err := b.Build(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestBuild_NoTraces(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-empty")

	_, _, err := b.Build(ctx, "run-empty")
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestBuild_VerifyIntegrity(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-verify")
	recordTraces(t, store, "run-verify", 4)

	_, _, err := b.Build(ctx, "run-verify")
	require.NoError(t, err)

	// Verify should pass on a freshly built chain.
	count, err := b.Verify(ctx, "run-verify")
	require.NoError(t, err)
	assert.Equal(t, 4, count)
}

func TestBuild_VerifyDetectsTamper(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-tamper")
	recordTraces(t, store, "run-tamper", 3)

	_, _, err := b.Build(ctx, "run-tamper")
	require.NoError(t, err)

	// Tamper with the middle trace's Detail.
	// The WORM trigger blocks content-field updates, so temporarily disable it
	// to simulate a bypass attack.
	withWORMTriggerDisabled(t, store, func() {
		traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tamper"})
		require.NoError(t, err)
		require.Len(t, traces, 3)
		traces[1].Detail = `{"tampered":true}`
		require.NoError(t, store.UpdateTrace(ctx, traces[1]))
	})

	// Verify should now fail with ErrHashMismatch.
	_, err = b.Verify(ctx, "run-tamper")
	require.ErrorIs(t, err, ErrHashMismatch)
}

func TestBuildBatch_ContinuousChain(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-batch")
	recordTraces(t, store, "run-batch", 5)

	// batchSize=2 means 3 batches: [0,1], [2,3], [4].
	count, tail, err := b.BuildBatch(ctx, "run-batch", 2)
	require.NoError(t, err)
	assert.Equal(t, 5, count)
	assert.NotEmpty(t, tail)

	// The chain must be continuous across batches.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-batch"})
	require.NoError(t, err)
	require.Len(t, traces, 5)

	assert.Empty(t, traces[0].PrevHash)
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash,
			"trace %d PrevHash should equal previous CurrHash (cross-batch continuity)", i)
	}
	assert.Equal(t, traces[len(traces)-1].CurrHash, tail)

	// Verify the whole chain.
	_, err = b.Verify(ctx, "run-batch")
	require.NoError(t, err)
}

func TestBuildBatch_BatchSizeOne(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-one")
	recordTraces(t, store, "run-one", 3)

	count, tail, err := b.BuildBatch(ctx, "run-one", 1)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.NotEmpty(t, tail)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-one"})
	require.NoError(t, err)
	require.Len(t, traces, 3)

	assert.Empty(t, traces[0].PrevHash)
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash)
	}
}

func TestBuildBatch_InvalidBatchSize(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-bs")
	recordTraces(t, store, "run-bs", 2)

	_, _, err := b.BuildBatch(ctx, "run-bs", 0)
	require.ErrorIs(t, err, ErrInvalidBatchSize)

	_, _, err = b.BuildBatch(ctx, "run-bs", -1)
	require.ErrorIs(t, err, ErrInvalidBatchSize)
}

func TestBuildBatch_EmptyRunID(t *testing.T) {
	b, _ := newChainBuilder(t)
	_, _, err := b.BuildBatch(context.Background(), "", 10)
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestBuildBatch_NoTraces(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-empty-batch")

	_, _, err := b.BuildBatch(ctx, "run-empty-batch", 10)
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestBuildBatch_EqualsBuild(t *testing.T) {
	// BuildBatch with a batch size larger than the trace count should produce
	// a valid continuous chain, equivalent in structure to Build.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-cmp")
	recordTraces(t, store, "run-cmp", 3)

	// BuildBatch with batchSize=100 (> trace count) behaves like Build.
	count, tail, err := b.BuildBatch(ctx, "run-cmp", 100)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.NotEmpty(t, tail)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-cmp"})
	require.NoError(t, err)
	require.Len(t, traces, 3)

	// Same chain structure as Build: first PrevHash empty, each subsequent
	// PrevHash equals previous CurrHash.
	assert.Empty(t, traces[0].PrevHash)
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash)
	}
	assert.Equal(t, traces[len(traces)-1].CurrHash, tail)

	// Verify passes.
	_, err = b.Verify(ctx, "run-cmp")
	require.NoError(t, err)
}

func TestComputeHash_Deterministic(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	trace := &state.Trace{
		ID:        "trace-1",
		RunID:     "run-1",
		Event:     EventStepExecute,
		Actor:     "system",
		Detail:    `{"target":"host:1"}`,
		Timestamp: now,
	}
	h1 := ComputeHash(trace, "")
	h2 := ComputeHash(trace, "")
	assert.Equal(t, h1, h2, "same inputs must produce same hash")
	assert.Len(t, h1, 64, "SHA-256 hex encoding is 64 chars")
}

func TestComputeHash_DifferentInputs(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	base := &state.Trace{
		ID:        "trace-1",
		RunID:     "run-1",
		Event:     EventStepExecute,
		Actor:     "system",
		Detail:    `{"target":"host:1"}`,
		Timestamp: now,
	}

	// Different prev hash.
	other := *base
	h1 := ComputeHash(base, "")
	h2 := ComputeHash(&other, "abcdef")
	assert.NotEqual(t, h1, h2, "different prev hash must produce different hash")

	// Different detail.
	other2 := *base
	other2.Detail = `{"target":"host:2"}`
	h3 := ComputeHash(&other2, "")
	assert.NotEqual(t, h1, h3, "different detail must produce different hash")

	// Different timestamp.
	other3 := *base
	other3.Timestamp = now.Add(time.Second)
	h4 := ComputeHash(&other3, "")
	assert.NotEqual(t, h1, h4, "different timestamp must produce different hash")
}

func TestComputeHash_NilTrace(t *testing.T) {
	// Should not panic; returns a stable hash.
	h := ComputeHash(nil, "")
	assert.Len(t, h, 64)
}

func TestBuild_TamperDetectedViaRebuild(t *testing.T) {
	// SA-002: After building the chain, tampering with a trace and then
	// calling Build must NOT succeed — Build must refuse to rebuild an
	// existing chain, preventing the attacker from legalizing tampered data.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-rebuild")
	recordTraces(t, store, "run-rebuild", 3)

	_, _, err := b.Build(ctx, "run-rebuild")
	require.NoError(t, err)

	// Tamper with the first trace's Detail.
	// The WORM trigger blocks content-field updates, so temporarily disable it
	// to simulate a bypass attack.
	withWORMTriggerDisabled(t, store, func() {
		origTraces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-rebuild"})
		require.NoError(t, err)
		require.Len(t, origTraces, 3)
		origTraces[0].Detail = `{"tampered":true}`
		require.NoError(t, store.UpdateTrace(ctx, origTraces[0]))
	})

	// Build must now refuse to rebuild because the chain exists but is broken.
	_, _, err = b.Build(ctx, "run-rebuild")
	require.ErrorIs(t, err, ErrChainBroken)
}

func TestBuild_MultipleRunsIndependent(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-a")
	createRun(t, store, "run-b")
	recordTraces(t, store, "run-a", 2)
	recordTraces(t, store, "run-b", 2)

	countA, tailA, err := b.Build(ctx, "run-a")
	require.NoError(t, err)
	assert.Equal(t, 2, countA)

	countB, tailB, err := b.Build(ctx, "run-b")
	require.NoError(t, err)
	assert.Equal(t, 2, countB)

	// The two chains are independent: their tail hashes differ (the trace
	// content differs) and each chain's first trace has an empty PrevHash.
	tracesA, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-a"})
	require.NoError(t, err)
	tracesB, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-b"})
	require.NoError(t, err)

	assert.Empty(t, tracesA[0].PrevHash)
	assert.Empty(t, tracesB[0].PrevHash)
	assert.NotEqual(t, tracesA[0].CurrHash, tracesB[0].CurrHash)
	assert.NotEqual(t, tailA, tailB)

	// Verifying each run independently should pass.
	_, err = b.Verify(ctx, "run-a")
	require.NoError(t, err)
	_, err = b.Verify(ctx, "run-b")
	require.NoError(t, err)
}

func TestVerify_EmptyRunID(t *testing.T) {
	b, _ := newChainBuilder(t)
	_, err := b.Verify(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestVerify_NoTraces(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-verify-empty")

	_, err := b.Verify(ctx, "run-verify-empty")
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestVerify_PrevHashMismatch(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-prev")
	recordTraces(t, store, "run-prev", 2)

	_, _, err := b.Build(ctx, "run-prev")
	require.NoError(t, err)

	// Corrupt the second trace's PrevHash via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-prev"})
	require.NoError(t, err)
	require.Len(t, traces, 2)
	tamperTracePrevHash(t, store, traces[1].ID, "tampered-prev")

	_, err = b.Verify(ctx, "run-prev")
	require.ErrorIs(t, err, ErrHashMismatch)
}

func TestBuild_ChainAlreadyBuilt_ReturnsError(t *testing.T) {
	// After a successful Build, calling Build again on the same run must
	// return ErrChainAlreadyBuilt because the chain is intact.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-already")
	recordTraces(t, store, "run-already", 3)

	_, _, err := b.Build(ctx, "run-already")
	require.NoError(t, err)

	// Second Build on the same run must be refused.
	_, _, err = b.Build(ctx, "run-already")
	require.ErrorIs(t, err, ErrChainAlreadyBuilt)
}

func TestBuild_ChainBroken_ReturnsError(t *testing.T) {
	// After building the chain, tampering with a trace and then calling
	// Build must return ErrChainBroken (not rebuild silently).
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-broken")
	recordTraces(t, store, "run-broken", 3)

	_, _, err := b.Build(ctx, "run-broken")
	require.NoError(t, err)

	// Tamper with a trace's Detail.
	// The WORM trigger blocks content-field updates, so temporarily disable it
	// to simulate a bypass attack.
	withWORMTriggerDisabled(t, store, func() {
		traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-broken"})
		require.NoError(t, err)
		require.Len(t, traces, 3)
		traces[1].Detail = `{"tampered":true}`
		require.NoError(t, store.UpdateTrace(ctx, traces[1]))
	})

	// Build must refuse because the chain is broken.
	_, _, err = b.Build(ctx, "run-broken")
	require.ErrorIs(t, err, ErrChainBroken)
}

func TestBuildForce_SucceedsAfterChainBuilt(t *testing.T) {
	// BuildForce should succeed even when the chain already exists.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-force")
	recordTraces(t, store, "run-force", 3)

	_, _, err := b.Build(ctx, "run-force")
	require.NoError(t, err)

	// BuildForce on the same run must succeed.
	count, tail, err := b.BuildForce(ctx, "run-force")
	require.NoError(t, err)
	assert.Equal(t, 3, count)
	assert.NotEmpty(t, tail)

	// The rebuilt chain must pass verification.
	_, err = b.Verify(ctx, "run-force")
	require.NoError(t, err)
}

func TestBuildForce_EmptyRunID(t *testing.T) {
	b, _ := newChainBuilder(t)
	_, _, err := b.BuildForce(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestBuildForce_NoTraces(t *testing.T) {
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-force-empty")

	_, _, err := b.BuildForce(ctx, "run-force-empty")
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestBuildBatch_ChainAlreadyBuilt_ReturnsError(t *testing.T) {
	// BuildBatch must also refuse to rebuild an intact chain.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-batch-already")
	recordTraces(t, store, "run-batch-already", 3)

	_, _, err := b.BuildBatch(ctx, "run-batch-already", 2)
	require.NoError(t, err)

	_, _, err = b.BuildBatch(ctx, "run-batch-already", 2)
	require.ErrorIs(t, err, ErrChainAlreadyBuilt)
}

func TestBuild_DeterministicOrder_SameTimestamp(t *testing.T) {
	// SA-008: Records sharing the same timestamp must be ordered deterministically
	// (by id ASC as a secondary sort key) so that the hash chain is reproducible.
	// Without the tie-breaker, SQLite returns rows in an unspecified order for
	// equal timestamps, producing a different chain on every build.
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-det")

	// Insert 4 traces that all share the same timestamp. The ids are deliberately
	// out of lexicographic order relative to insertion to prove that id ASC
	// (not insertion order) drives the chain order.
	sameTS := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	insertIDs := []string{"trace-d", "trace-a", "trace-c", "trace-b"}
	for i, id := range insertIDs {
		trace := &state.Trace{
			ID:        id,
			RunID:     "run-det",
			Event:     EventStepExecute,
			Actor:     "system",
			Detail:    fmt.Sprintf(`{"index":%d}`, i),
			Timestamp: sameTS,
		}
		require.NoError(t, store.CreateTrace(ctx, trace))
	}

	// First build.
	_, _, err := b.BuildForce(ctx, "run-det")
	require.NoError(t, err)

	traces1, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-det"})
	require.NoError(t, err)
	require.Len(t, traces1, 4)

	// Capture the id order and hash chain from the first build.
	firstIDs := make([]string, len(traces1))
	firstHashes := make([]string, len(traces1))
	for i, tr := range traces1 {
		firstIDs[i] = tr.ID
		firstHashes[i] = tr.CurrHash
	}

	// The ids must be sorted ascending (the secondary sort key), regardless of
	// the insertion order.
	expectedIDs := []string{"trace-a", "trace-b", "trace-c", "trace-d"}
	assert.Equal(t, expectedIDs, firstIDs,
		"traces must be ordered by id ASC when timestamps are equal")

	// Second build (force rebuild) must produce the exact same chain.
	_, _, err = b.BuildForce(ctx, "run-det")
	require.NoError(t, err)

	traces2, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-det"})
	require.NoError(t, err)
	require.Len(t, traces2, 4)

	for i, tr := range traces2 {
		assert.Equal(t, firstIDs[i], tr.ID,
			"id order must be stable across rebuilds")
		assert.Equal(t, firstHashes[i], tr.CurrHash,
			"CurrHash must be identical across rebuilds (deterministic chain)")
	}

	// The chain must verify cleanly.
	count, err := b.Verify(ctx, "run-det")
	require.NoError(t, err)
	assert.Equal(t, 4, count)
}
