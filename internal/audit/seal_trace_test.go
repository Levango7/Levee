package audit

// Tests for SealRunTraceChain and HashChainBuilder.Seal — the trace chain's
// production seal. Two things they exist to pin down:
//
//   - The seal is safe to run unattended on the write path. TraceRecorder.Record
//     calls it after every append, and every settle funnel calls it again, so it
//     must be idempotent, must extend the chain over a late record rather than
//     break it, and must survive a lost race with a concurrent seal.
//   - It cannot repair a chain that does not verify. Recomputing would relink a
//     tampered row and destroy the only evidence of the tamper, so Seal reports
//     instead. This is where the trace chain deliberately differs from the audit
//     chain, whose Seal rewrites any row whose hash differs.

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func newSealTestStore(t *testing.T) (*state.SQLiteStore, *TraceRecorder) {
	t.Helper()
	store := newTestStore(t)
	createRun(t, store, "run-seal")
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)
	return store, rec
}

// recordN writes n traces for runID through the production recorder, which
// seals after each one.
func recordN(t *testing.T, rec *TraceRecorder, runID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tr, err := rec.Record(ctx, TraceRecord{
			RunID: runID, Event: EventStepExecute, Actor: "system",
			Output: map[string]any{"i": i},
		})
		require.NoError(t, err)
		ids = append(ids, tr.ID)
	}
	return ids
}

func TestSeal_RecordedRunIsSealedAndVerifies(t *testing.T) {
	// The headline behaviour: a run that is still in flight has a verifiable
	// chain, because every recorded trace sealed the run behind it.
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	ids := recordN(t, rec, "run-seal", 3)
	require.Len(t, ids, 3)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	assert.True(t, result.Valid, "an in-flight run must verify: %+v", result.Failures)
	assert.Equal(t, 3, result.Count)
	assert.Empty(t, result.Failures)

	// And the chain is genuinely linked, not merely non-empty.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-seal"})
	require.NoError(t, err)
	assert.Empty(t, traces[0].PrevHash)
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash)
		assert.NotEmpty(t, traces[i].CurrHash)
	}
}

func TestSeal_IsIdempotent(t *testing.T) {
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	recordN(t, rec, "run-seal", 3)

	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)

	first, err := b.Seal(ctx, "run-seal")
	require.NoError(t, err)
	assert.Equal(t, 3, first.Count)
	assert.Equal(t, 0, first.Sealed, "the recorder already sealed these rows")

	// The settle-time call re-seals: nothing to write, same tail.
	second, err := b.Seal(ctx, "run-seal")
	require.NoError(t, err)
	assert.Equal(t, 0, second.Sealed)
	assert.Equal(t, first.TailHash, second.TailHash)

	// SealRunTraceChain, the form the funnels call, is a no-op here too.
	require.NoError(t, SealRunTraceChain(ctx, store, "run-seal"))
}

// TestSeal_LateAppendExtendsTheChain is the case that made a settle-time-only
// seal untenable. A trace written after the chain was closed used to leave Build
// refusing forever, so /audit/verify reported the run as tampered for good. It
// must instead extend the chain.
func TestSeal_LateAppendExtendsTheChain(t *testing.T) {
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	recordN(t, rec, "run-seal", 2)

	// The run settles here.
	require.NoError(t, SealRunTraceChain(ctx, store, "run-seal"))

	// Something writes one more trace afterwards.
	late := recordN(t, rec, "run-seal", 1)
	require.Len(t, late, 1)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	assert.True(t, result.Valid,
		"a late trace must extend the chain, not break it: %+v", result.Failures)
	assert.Equal(t, 3, result.Count)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-seal"})
	require.NoError(t, err)
	assert.NotEmpty(t, traces[2].CurrHash, "the late record must itself be sealed")
	assert.Equal(t, traces[1].CurrHash, traces[2].PrevHash)
}

// TestSeal_DoesNotRepairATamperedChain is the property the audit chain's Seal
// does not have. A sealed row whose content was altered behind the WORM trigger
// must stay broken and stay reported: relinking it would launder the tamper.
func TestSeal_DoesNotRepairATamperedChain(t *testing.T) {
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	ids := recordN(t, rec, "run-seal", 3)

	// An attacker with direct database access edits a sealed row's content.
	tamperTraceDetail(t, store, ids[1], `{"tampered":true}`)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	before, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	require.False(t, before.Valid, "tampering must be visible before any seal")

	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, err = b.Seal(ctx, "run-seal")
	require.Error(t, err, "seal must report a chain it will not repair")
	assert.ErrorIs(t, err, ErrChainBroken)

	// Still broken afterwards — the seal did not paper over it.
	after, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	assert.False(t, after.Valid, "the evidence must survive a seal attempt")
	assert.Len(t, after.Failures, 1)
	assert.Equal(t, ids[1], after.Failures[0].TraceID)

	// The administrative override is still available and does repair it.
	_, _, err = b.BuildForce(ctx, "run-seal")
	require.NoError(t, err)
	repaired, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	assert.True(t, repaired.Valid, "BuildForce is the deliberate recovery path")
}

// TestSeal_FillsAHoleLeftByAFailedSeal covers the unsealed tail: a seal that
// died partway leaves rows without hashes, and the next seal closes them.
func TestSeal_FillsAHoleLeftByAFailedSeal(t *testing.T) {
	store := newTestStore(t)
	createRun(t, store, "run-seal")
	recordTraces(t, store, "run-seal", 4) // rows with no chain at all

	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	seal, err := b.Seal(context.Background(), "run-seal")
	require.NoError(t, err)
	assert.Equal(t, 4, seal.Count)
	assert.Equal(t, 4, seal.Sealed, "every unsealed row is written")

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(context.Background(), "run-seal")
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
}

func TestSeal_ConcurrentSealsAreSafe(t *testing.T) {
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	recordN(t, rec, "run-seal", 4)

	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)

	// Two overlapping seals, as a lost settle race produces.
	var wg sync.WaitGroup
	results := make([]*SealResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = b.Seal(ctx, "run-seal")
		}(i)
	}
	wg.Wait()
	for i := range errs {
		require.NoError(t, errs[i])
	}
	assert.Equal(t, results[0].TailHash, results[1].TailHash,
		"overlapping seals must agree on the tail")

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-seal")
	require.NoError(t, err)
	assert.True(t, result.Valid, "failures: %+v", result.Failures)
}

func TestSealRunTraceChain_NoTracesIsNotAnError(t *testing.T) {
	// A run rejected or cancelled before apply has no traces; every funnel
	// calls the seal regardless, and must not have to dedupe an error.
	store := newTestStore(t)
	createRun(t, store, "run-empty")

	require.NoError(t, SealRunTraceChain(context.Background(), store, "run-empty"))
}

func TestSeal_InputGuards(t *testing.T) {
	store, _ := newSealTestStore(t)
	b, err := NewHashChainBuilder(store)
	require.NoError(t, err)

	_, err = b.Seal(context.Background(), "")
	require.ErrorIs(t, err, ErrEmptyRunID)

	require.ErrorIs(t, SealRunTraceChain(context.Background(), nil, "run-seal"), ErrNilStore)
}

// TestSeal_OnlyWritesChainColumns pins the write surface. The seal must not be
// able to touch a content column even if the hash it computed disagrees — here
// the row is rewritten to prove UpdateTraceChain writes prev/curr and nothing
// else.
func TestSeal_WritesOnlyChainColumns(t *testing.T) {
	store, rec := newSealTestStore(t)
	ctx := context.Background()
	ids := recordN(t, rec, "run-seal", 2)

	before, err := store.GetTrace(ctx, ids[0])
	require.NoError(t, err)
	require.NotNil(t, before)
	snapshot := *before

	require.NoError(t, store.UpdateTraceChain(ctx, ids[0], "prev-x", "curr-y"))

	after, err := store.GetTrace(ctx, ids[0])
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "prev-x", after.PrevHash)
	assert.Equal(t, "curr-y", after.CurrHash)
	assert.Equal(t, snapshot.ID, after.ID)
	assert.Equal(t, snapshot.RunID, after.RunID)
	assert.Equal(t, snapshot.Event, after.Event)
	assert.Equal(t, snapshot.Actor, after.Actor)
	assert.Equal(t, snapshot.Detail, after.Detail)
	assert.True(t, snapshot.Timestamp.Equal(after.Timestamp),
		"a chain stamp must not move the timestamp — it is hashed content")

	// An unknown id is an error, never a silent success: a no-op here would
	// leave a hole that only surfaces at verification time.
	require.Error(t, store.UpdateTraceChain(ctx, "trc-does-not-exist", "p", "c"))
}
