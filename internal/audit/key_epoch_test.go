package audit

// D-8 second dimension: HMAC key epoch. A keyed digest carries a short key
// fingerprint ("k<8hex>:") so the verifier can tell "operator rotated the key"
// (ErrKeyEpochMismatch / FailureKeyEpochMismatch) apart from "chain tampered"
// (ErrHashMismatch / FailureHashMismatch).

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

const keyA = "0123456789abcdef0123456789abcdef"
const keyB = "fedcba9876543210fedcba9876543210"

func TestKeyEpochTagPresentAndDistinct(t *testing.T) {
	setHMACKey(t, keyA)
	da := digestWithMode("payload", digestModeSHA256)
	ea := keyEpochID()
	assert.True(t, strings.HasPrefix(da, "k"+ea+":"), "keyed digest must carry epoch tag %q", ea)

	setHMACKey(t, keyB)
	db := digestWithMode("payload", digestModeSHA256)
	eb := keyEpochID()
	assert.NotEqual(t, ea, eb, "different keys must have different epochs")
	assert.True(t, strings.HasPrefix(db, "k"+eb+":"))
	assert.NotEqual(t, da, db)
}

func TestKeyEpochMismatchOnRotation(t *testing.T) {
	// Digest under key A.
	setHMACKey(t, keyA)
	stored := digestWithMode("payload", digestModeSHA256)

	// Rotate to key B: recompute must report ErrKeyEpochMismatch, not a hash
	// mismatch (which would be indistinguishable from tampering).
	setHMACKey(t, keyB)
	_, err := recomputeCanonical("payload", stored)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyEpochMismatch)
	assert.NotErrorIs(t, err, ErrHashMismatch)
}

func TestKeyEpochMismatchOnKeyRemoved(t *testing.T) {
	setHMACKey(t, keyA)
	stored := digestWithMode("payload", digestModeSHA256)
	unsetHMACKey(t)
	_, err := recomputeCanonical("payload", stored)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyEpochMismatch)
}

func TestUnkeyedHasNoEpochTag(t *testing.T) {
	unsetHMACKey(t)
	d := digestWithMode("payload", digestModeSHA256)
	assert.Len(t, d, 64)
	assert.False(t, strings.HasPrefix(d, "k"))
	got, err := recomputeCanonical("payload", d)
	require.NoError(t, err)
	assert.Equal(t, d, got)
}

func TestSM3KeyedEpochRoundTrip(t *testing.T) {
	setHMACKey(t, keyA)
	stored := digestWithMode("payload", digestModeSM3)
	assert.True(t, strings.HasPrefix(stored, prefixHMACSM3))
	got, err := recomputeCanonical("payload", stored)
	require.NoError(t, err)
	assert.Equal(t, stored, got, "SM3 keyed digest must round-trip with its epoch tag")

	setHMACKey(t, keyB)
	_, err = recomputeCanonical("payload", stored)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyEpochMismatch)
}

func TestKeyEpochFailureTypeInChain(t *testing.T) {
	// Build a keyed chain under key A (tagged rows), then verify under key B.
	setHMACKey(t, keyA)
	useMode(t, digestModeSHA256)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	const runID = "run-key-epoch"
	createRun(t, store, runID)
	recordTraces(t, store, runID, 3)
	_, _, err := b.Build(ctx, runID)
	require.NoError(t, err)

	// Rows carry the epoch tag of key A.
	fresh, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(fresh[0].CurrHash, keyEpochTag()))

	// Rotate key: VerifyTraces must report FailureKeyEpochMismatch (not tampering).
	setHMACKey(t, keyB)
	fresh2, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	result := VerifyTraces(runID, fresh2)
	assert.False(t, result.Valid)
	require.NotEmpty(t, result.Failures)
	assert.Equal(t, FailureKeyEpochMismatch, result.Failures[0].Type)

	// Builder Verify wraps ErrKeyEpochMismatch distinctly.
	_, err = b.Verify(ctx, runID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrKeyEpochMismatch)
}

func TestFailureTypeString_KeyEpoch(t *testing.T) {
	assert.Equal(t, "key_epoch_mismatch", FailureKeyEpochMismatch.String())
	assert.Equal(t, "hash_mismatch", FailureHashMismatch.String())
	assert.Equal(t, "prev_hash_mismatch", FailurePrevHashMismatch.String())
	assert.Equal(t, "empty_hash", FailureEmptyHash.String())
}

// Regression (code-review Spec axis): a chain written with a key BEFORE key
// epochs existed stored BARE HMAC-SHA256 hex (no "k<fp>:" tag). With the SAME
// key still configured, it must remain verifiable — not misreported as
// tampering. Only a DIFFERENT key should break it (and even then as a bare-hash
// mismatch, since there is no epoch tag to tell key-rotation from tampering for
// pre-epoch rows).
func TestSameKeyPreEpochKeyedChainStillVerifies(t *testing.T) {
	unsetHMACKey(t)
	setHMACKey(t, keyA)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	const runID = "run-preepoch-keyed"
	createRun(t, store, runID)
	recordTraces(t, store, runID, 3)

	// Simulate pre-epoch storage: compute each CurrHash with the OLD bare HMAC
	// formula (no epoch tag) and persist it directly, bypassing ComputeHash.
	fresh, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	prev := ""
	for i := range fresh {
		payload := traceCanonical(fresh[i], prev)
		fresh[i].PrevHash = prev
		fresh[i].CurrHash = legacyKeyedDigest(payload) // bare HMAC, no tag
		require.NotEmpty(t, fresh[i].CurrHash)
		require.NoError(t, store.UpdateTraceChain(ctx, fresh[i].ID, prev, fresh[i].CurrHash))
		prev = fresh[i].CurrHash
	}

	// Verify with the SAME key: must pass (regression guard).
	readBack, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	result := VerifyTraces(runID, readBack)
	assert.True(t, result.Valid, "same-key pre-epoch keyed chain must still verify: %+v", result.Failures)

	// Builder path uses the same verifier.
	count, err := b.Verify(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}
