package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newVerifier builds a ChainVerifier on top of a fresh temp-file store and
// returns the underlying store so the test can insert runs/traces.
func newVerifier(t *testing.T) (*ChainVerifier, *state.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	v, err := NewChainVerifier(store)
	require.NoError(t, err)
	return v, store
}

// buildChain is a test helper that records n traces for runID, builds the
// hash chain and returns the tail hash. It fails the test on any error.
func buildChain(t *testing.T, store state.Store, runID string, n int) string {
	t.Helper()
	ctx := context.Background()
	createRun(t, store, runID)
	recordTraces(t, store, runID, n)
	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, tail, err := b.Build(ctx, runID)
	require.NoError(t, err)
	return tail
}

func TestNewChainVerifier_NilStore(t *testing.T) {
	v, err := NewChainVerifier(nil)
	require.ErrorIs(t, err, ErrNilStore)
	assert.Nil(t, v)
}

func TestChainVerify_Success(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-ok", 3)

	result, err := v.Verify(ctx, "run-ok")
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Equal(t, "run-ok", result.RunID)
	assert.Equal(t, 3, result.Count)
	assert.Empty(t, result.Failures)
}

func TestChainVerify_TamperDetail(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-detail", 3)

	// Tamper with the middle trace's Detail via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-detail"})
	require.NoError(t, err)
	require.Len(t, traces, 3)
	tamperTraceDetail(t, store, traces[1].ID, `{"tampered":true}`)

	result, err := v.Verify(ctx, "run-detail")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 1)

	f := result.Failures[0]
	assert.Equal(t, traces[1].ID, f.TraceID)
	assert.Equal(t, 1, f.Index)
	assert.Equal(t, FailureHashMismatch, f.Type)
	assert.NotEqual(t, f.Expected, f.Actual)
	// PrevHash continuity is intact: only the content (Detail) was tampered,
	// so PrevExpected equals PrevActual.
	assert.Equal(t, f.PrevExpected, f.PrevActual)
}

func TestChainVerify_TamperCurrHash(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-curr", 3)

	// Directly overwrite the last trace's CurrHash via raw SQL (bypassing
	// WORM trigger). Tampering the last record avoids a cascading
	// PrevHashMismatch on the next record (there is no next record), so
	// exactly one failure is reported.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-curr"})
	require.NoError(t, err)
	require.Len(t, traces, 3)
	last := len(traces) - 1
	originalHash := traces[last].CurrHash
	tamperTraceCurrHash(t, store, traces[last].ID, "deadbeef"+originalHash[:0])

	result, err := v.Verify(ctx, "run-curr")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 1)

	f := result.Failures[0]
	assert.Equal(t, traces[last].ID, f.TraceID)
	assert.Equal(t, last, f.Index)
	assert.Equal(t, FailureHashMismatch, f.Type)
	assert.Equal(t, originalHash, f.Expected)
	assert.NotEqual(t, f.Expected, f.Actual)
}

func TestChainVerify_PrevHashBreak(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-prev", 3)

	// Corrupt the middle trace's PrevHash via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-prev"})
	require.NoError(t, err)
	require.Len(t, traces, 3)
	expectedPrev := traces[1].PrevHash
	tamperTracePrevHash(t, store, traces[1].ID, "tampered-prev-hash-value")

	result, err := v.Verify(ctx, "run-prev")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 1)

	f := result.Failures[0]
	assert.Equal(t, traces[1].ID, f.TraceID)
	assert.Equal(t, 1, f.Index)
	assert.Equal(t, FailurePrevHashMismatch, f.Type)
	assert.Equal(t, expectedPrev, f.PrevExpected)
	assert.Equal(t, "tampered-prev-hash-value", f.PrevActual)
}

func TestChainVerify_EmptyRun(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	createRun(t, store, "run-empty")

	_, err := v.Verify(ctx, "run-empty")
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestChainVerify_EmptyRunID(t *testing.T) {
	v, _ := newVerifier(t)
	_, err := v.Verify(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestChainVerify_UnbuiltChain(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	createRun(t, store, "run-unbuilt")
	// Record traces but do NOT build the chain: CurrHash stays empty.
	recordTraces(t, store, "run-unbuilt", 3)

	result, err := v.Verify(ctx, "run-unbuilt")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 3)

	for i, f := range result.Failures {
		assert.Equal(t, i, f.Index)
		assert.Equal(t, FailureEmptyHash, f.Type, "trace %d should be empty hash", i)
		assert.Empty(t, f.Actual, "stored CurrHash should be empty")
		assert.NotEmpty(t, f.Expected, "recomputed hash should be non-empty")
	}
}

func TestChainVerifyStrict_Success(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-strict-ok", 2)

	err := v.VerifyStrict(ctx, "run-strict-ok")
	assert.NoError(t, err)
}

func TestChainVerifyStrict_Failure(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-strict-fail", 2)

	// Tamper with the first trace's Detail via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-strict-fail"})
	require.NoError(t, err)
	tamperTraceDetail(t, store, traces[0].ID, `{"tampered":true}`)

	err = v.VerifyStrict(ctx, "run-strict-fail")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrVerifyFailed))
	assert.False(t, errors.Is(err, ErrNoTraces))
}

func TestChainVerifyStrict_EmptyRunID(t *testing.T) {
	v, _ := newVerifier(t)
	err := v.VerifyStrict(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestChainVerifyStrict_NoTraces(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	createRun(t, store, "run-strict-empty")

	err := v.VerifyStrict(ctx, "run-strict-empty")
	require.ErrorIs(t, err, ErrNoTraces)
}

func TestChainVerify_AllTampered(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-all", 4)

	// Tamper with every trace's Detail via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-all"})
	require.NoError(t, err)
	require.Len(t, traces, 4)
	for i, tr := range traces {
		tamperTraceDetail(t, store, tr.ID, `{"tampered":`+string(rune('0'+i))+`}`)
	}

	result, err := v.Verify(ctx, "run-all")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	// Every record's content changed, so every record reports a failure.
	require.Len(t, result.Failures, 4)
	for i, f := range result.Failures {
		assert.Equal(t, i, f.Index)
		assert.Equal(t, FailureHashMismatch, f.Type)
		assert.NotEqual(t, f.Expected, f.Actual)
	}
}

func TestChainVerifyResult_Fields(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-fields", 5)

	// Tamper with traces at index 1 and 3 via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-fields"})
	require.NoError(t, err)
	require.Len(t, traces, 5)
	tamperTraceDetail(t, store, traces[1].ID, `{"t":1}`)
	tamperTraceDetail(t, store, traces[3].ID, `{"t":3}`)

	result, err := v.Verify(ctx, "run-fields")
	require.NoError(t, err)

	assert.Equal(t, "run-fields", result.RunID)
	assert.Equal(t, 5, result.Count)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 2)
	assert.Equal(t, 1, result.Failures[0].Index)
	assert.Equal(t, 3, result.Failures[1].Index)
}

func TestChainVerify_TailHashConsistent(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	tail := buildChain(t, store, "run-tail", 3)

	// The last trace's CurrHash must equal the Build-returned tail hash.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tail"})
	require.NoError(t, err)
	require.Len(t, traces, 3)
	assert.Equal(t, tail, traces[len(traces)-1].CurrHash)

	// Verification passes and the chain is intact.
	result, err := v.Verify(ctx, "run-tail")
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Equal(t, 3, result.Count)
}

func TestChainVerify_SingleTrace(t *testing.T) {
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-single", 1)

	result, err := v.Verify(ctx, "run-single")
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Equal(t, 1, result.Count)
	assert.Empty(t, result.Failures)
}

func TestChainVerify_TamperCurrHashBreaksPrevContinuity(t *testing.T) {
	// When a middle trace's CurrHash is directly overwritten, that trace
	// reports a hash mismatch AND the next trace reports a prev-hash
	// mismatch (its PrevHash no longer equals the overwritten CurrHash).
	v, store := newVerifier(t)
	ctx := context.Background()
	buildChain(t, store, "run-cascade", 3)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-cascade"})
	require.NoError(t, err)
	require.Len(t, traces, 3)

	// Overwrite the first trace's CurrHash with a different non-empty value
	// via raw SQL (bypassing WORM trigger).
	traces[0].CurrHash = "aaaa" + traces[0].CurrHash[4:]
	tamperTraceCurrHash(t, store, traces[0].ID, traces[0].CurrHash)

	result, err := v.Verify(ctx, "run-cascade")
	require.NoError(t, err)
	assert.False(t, result.Valid)
	require.Len(t, result.Failures, 2)

	// First failure: the tampered trace's CurrHash no longer matches the
	// recomputed hash.
	assert.Equal(t, 0, result.Failures[0].Index)
	assert.Equal(t, FailureHashMismatch, result.Failures[0].Type)

	// Second failure: the next trace's PrevHash (still the original hash)
	// does not equal the overwritten CurrHash.
	assert.Equal(t, 1, result.Failures[1].Index)
	assert.Equal(t, FailurePrevHashMismatch, result.Failures[1].Type)
}

func TestFailureType_String(t *testing.T) {
	assert.Equal(t, "hash_mismatch", FailureHashMismatch.String())
	assert.Equal(t, "prev_hash_mismatch", FailurePrevHashMismatch.String())
	assert.Equal(t, "empty_hash", FailureEmptyHash.String())
	assert.Equal(t, "unknown", FailureType(99).String())
}
