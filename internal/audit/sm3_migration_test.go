package audit

// D-8 audit-chain SM3 migration tests. The prefix scheme stores the hash
// family inside the digest value, so a single chain can mix SHA-256 rows and
// SM3 rows, and verification uses each row's own prefix — never the process
// configuration.

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/gmcrypto"
	"github.com/nexus/levee/internal/state"
)

func useMode(t *testing.T, mode string) {
	t.Helper()
	ResetDigestModeForTest()
	t.Setenv(EnvDigestMode, mode)
}

func TestDigestWithMode(t *testing.T) {
	unsetHMACKey(t)
	plain := digestWithMode("payload", digestModeSHA256)
	assert.Len(t, plain, 64, "sha256 must stay bare 64-hex")
	_, err := hex.DecodeString(plain)
	require.NoError(t, err)

	sm := digestWithMode("payload", digestModeSM3)
	assert.True(t, strings.HasPrefix(sm, prefixSM3), "sm3 must carry prefix")
	_, err = hex.DecodeString(strings.TrimPrefix(sm, prefixSM3))
	require.NoError(t, err)
	assert.Len(t, strings.TrimPrefix(sm, prefixSM3), 64)

	assert.NotEqual(t, plain, strings.TrimPrefix(sm, prefixSM3))
	assert.Panics(t, func() { digestWithMode("x", "md5") })
}

func TestDigestWithModeKeyed(t *testing.T) {
	setHMACKey(t, "0123456789abcdef0123456789abcdef")
	plain := digestWithMode("payload", digestModeSHA256)
	assert.Len(t, plain, 64, "keyed sha256 stays bare 64-hex")

	sm := digestWithMode("payload", digestModeSM3)
	assert.True(t, strings.HasPrefix(sm, prefixHMACSM3), "keyed sm3 uses hmac-sm3 prefix")

	unsetHMACKey(t)
	smUnkeyed := digestWithMode("payload", digestModeSM3)
	assert.True(t, strings.HasPrefix(smUnkeyed, prefixSM3))
	assert.NotEqual(t, sm, smUnkeyed, "keyed and unkeyed SM3 must differ")
}

func TestRecomputeCanonicalByPrefix(t *testing.T) {
	unsetHMACKey(t)
	payload := "canonical-payload"
	for _, mode := range []string{digestModeSHA256, digestModeSM3} {
		stored := digestWithMode(payload, mode)
		got, err := recomputeCanonical(payload, stored)
		require.NoError(t, err)
		assert.Equal(t, stored, got, "recompute must be self-consistent for %s", mode)
	}
	_, err := recomputeCanonical(payload, "md5:abcdef")
	require.Error(t, err)
}

func TestMixedChainVerifies(t *testing.T) {
	unsetHMACKey(t)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	const runID = "run-sm3-mixed"
	createRun(t, store, runID)
	recordTraces(t, store, runID, 6)

	fresh, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	require.Len(t, fresh, 6)

	// Build each row under an explicit family, keeping the prev linkage.
	useMode(t, digestModeSHA256)
	prev := ""
	for i := 0; i < 3; i++ {
		fresh[i].PrevHash = prev
		fresh[i].CurrHash = ComputeHash(fresh[i], prev) // sha256, no prefix
		prev = fresh[i].CurrHash
		require.NoError(t, store.UpdateTraceChain(ctx, fresh[i].ID, fresh[i].PrevHash, fresh[i].CurrHash))
	}
	useMode(t, digestModeSM3)
	for i := 3; i < len(fresh); i++ {
		fresh[i].PrevHash = prev
		fresh[i].CurrHash = ComputeHash(fresh[i], prev) // sm3, prefixed
		prev = fresh[i].CurrHash
		require.NoError(t, store.UpdateTraceChain(ctx, fresh[i].ID, fresh[i].PrevHash, fresh[i].CurrHash))
	}

	// Verify under a DIFFERENT configured mode to prove verification is
	// row-driven (each row's own prefix), not config-driven.
	useMode(t, digestModeSHA256)
	readBack, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	result := VerifyTraces(runID, readBack)
	assert.True(t, result.Valid, "mixed chain must verify: %+v", result.Failures)
	assert.Empty(t, result.Failures)
	assert.Equal(t, 0, result.LegacyCount)

	count, err := b.Verify(ctx, runID)
	require.NoError(t, err)
	assert.Equal(t, 6, count)
}

func TestSM3ChainDetectsTampering(t *testing.T) {
	unsetHMACKey(t)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	const runID = "run-sm3-tamper"
	createRun(t, store, runID)
	recordTraces(t, store, runID, 3)

	useMode(t, digestModeSM3)
	_, _, err := b.Build(ctx, runID)
	require.NoError(t, err)

	fresh, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(fresh[0].CurrHash, prefixSM3), "built row must be sm3-prefixed")

	fresh[1].Detail = fresh[1].Detail + "tampered"
	result := VerifyTraces(runID, fresh)
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Failures)
}

func TestSM3AuditChainVerifies(t *testing.T) {
	unsetHMACKey(t)
	ab, store := newAuditChainBuilder(t)
	ctx := context.Background()

	// Record() seals on write with the configured mode.
	useMode(t, digestModeSM3)
	ids := writeAudits(t, store, "aud-sm3", 3, time.Now().UTC())

	row, err := store.GetAudit(ctx, ids[0])
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(row.CurrHash, prefixSM3), "audit row must be sm3-prefixed")

	// Verify under a different configured mode — must be row-driven.
	useMode(t, digestModeSHA256)
	res, err := ab.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, res.Valid, "audit chain must verify: %+v", res.Failures)
}

func TestSM3KnownAnswerViaAudit(t *testing.T) {
	s := gmcrypto.SM3Sum([]byte("abc"))
	assert.Equal(t,
		"66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0",
		hex.EncodeToString(s[:]))
}
