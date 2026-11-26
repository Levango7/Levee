package audit

// D-8 audit-chain HMAC key epoch tests.
//
// The gap this closes: the digest algorithm family is self-describing (an SM3
// row carries "sm3:"/"hmac-sm3:" and verifies under its own family), but the
// SHA-256 family had no equivalent marker for KEYING. A keyed HMAC-SHA256 row
// and a historical unkeyed SHA-256 row were both bare 64-hex, so the verifier
// could not tell "this chain predates the key" from "someone rewrote it".
// Consequence measured in the ledger: adding LEVEE_AUDIT_HMAC_KEY to a
// deployment with history made every historical row fail as tampered.
//
// The fix follows the pattern this package already proved for algorithm
// migration (docs/audit-chain-mode-migration-design.md §10): mark the era in
// the digest VALUE, so historical rows need no backfill and keep verifying
// under the semantics they were written with.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// keyEpochTestKey is a 32-byte raw passphrase, comfortably over minHMACKeyLen.
const keyEpochTestKey = "0123456789abcdef0123456789abcdef"

// TestKeyedSHA256_DigestCarriesHMACPrefix specifies the write side: a keyed
// SHA-256 digest must declare its era, exactly as SM3 already does.
func TestKeyedSHA256_DigestCarriesHMACPrefix(t *testing.T) {
	unsetHMACKey(t)
	useMode(t, digestModeSHA256)

	unkeyed := digestWithMode("payload", digestModeSHA256)
	require.Len(t, unkeyed, 64, "unkeyed sha256 must stay bare 64-hex (historical shape)")
	_, err := hex.DecodeString(unkeyed)
	require.NoError(t, err)

	setHMACKey(t, keyEpochTestKey)
	keyed := digestWithMode("payload", digestModeSHA256)

	assert.True(t, strings.HasPrefix(keyed, prefixHMACSHA256),
		"a keyed sha256 digest must carry the hmac-sha256 prefix so its era is self-describing")
	body := strings.TrimPrefix(keyed, prefixHMACSHA256)
	_, err = hex.DecodeString(body)
	require.NoError(t, err)
	assert.Len(t, body, 64)
	assert.NotEqual(t, unkeyed, keyed, "keying must still change the digest value")
}

// TestRecomputeCanonical_KeyedSHA256Prefix_RoundTrips specifies the verify
// side for marked rows: dispatch on the prefix, and require the key.
func TestRecomputeCanonical_KeyedSHA256Prefix_RoundTrips(t *testing.T) {
	useMode(t, digestModeSHA256)
	setHMACKey(t, keyEpochTestKey)

	keyed := digestWithMode("payload", digestModeSHA256)
	got, err := recomputeCanonical("payload", keyed)
	require.NoError(t, err)
	assert.Equal(t, keyed, got, "a keyed row must round-trip under recomputation")

	unsetHMACKey(t)
	_, err = recomputeCanonical("payload", keyed)
	require.Error(t, err, "a keyed row must not verify once the key is gone")
	assert.Contains(t, err.Error(), EnvHMACKey,
		"the error must name the missing variable so an operator knows what to restore")
}

// TestRecomputeCanonical_UnprefixedRowIsUnkeyedEra is the behavioural core of
// the fix: an unprefixed row was written before keying and must be verified as
// unkeyed SHA-256 even though a key is now configured.
func TestRecomputeCanonical_UnprefixedRowIsUnkeyedEra(t *testing.T) {
	useMode(t, digestModeSHA256)

	// Written while unkeyed, as every row of a historical deployment was.
	unsetHMACKey(t)
	unkeyed := digestWithMode("payload", digestModeSHA256)
	require.Len(t, unkeyed, 64)

	// Now the operator hardens the deployment.
	setHMACKey(t, keyEpochTestKey)

	got, err := recomputeCanonical("payload", unkeyed)
	require.NoError(t, err, "an unprefixed historical row must not be recomputed with the new key")
	assert.Equal(t, unkeyed, got)

	// Guard the premise of the whole test: the two eras really differ.
	keyed := digestWithMode("payload", digestModeSHA256)
	assert.NotEqual(t, unkeyed, keyed)
}

// TestLegacyHash_IsUnkeyedEvenWhenKeyed pins the same discipline on the V1
// fallback. The SM3 branch already documents it ("must NOT silently switch to
// keyed just because a key is now configured — that would be the D-8 failure
// mode"); the legacy SHA-256 path had the opposite behaviour.
func TestLegacyHash_IsUnkeyedEvenWhenKeyed(t *testing.T) {
	useMode(t, digestModeSHA256)
	setHMACKey(t, keyEpochTestKey)

	tr := &state.Trace{ID: "id", RunID: "run", Event: "ev", Actor: "act", Detail: "det"}

	// digest() follows the current key, so the unkeyed expectation is computed
	// straight from the standard library rather than through the funnel.
	unkeyedOf := func(payload string) string {
		sum := sha256.Sum256([]byte(payload))
		return hex.EncodeToString(sum[:])
	}
	assert.Equal(t, unkeyedOf(canonicalV1(append([]string{""}, contentFields(tr)...)...)),
		legacyHash(tr, ""), "V1 rows predate keying and must be recomputed unkeyed")

	// Same for the WORM content checksum's V1 fallback.
	assert.Equal(t, unkeyedOf(canonicalV1(contentFields(tr)...)),
		legacyChecksum(tr), "V1 checksums predate keying and must be recomputed unkeyed")
}

// TestVerifyTraces_ReportsUnkeyedEraCount specifies operator visibility: after
// keying an existing deployment, the number of rows that predate the key must
// be reported rather than silently absorbed.
func TestVerifyTraces_ReportsUnkeyedEraCount(t *testing.T) {
	useMode(t, digestModeSHA256)
	unsetHMACKey(t)

	// A chain written while unkeyed.
	v, store := newVerifier(t)
	buildChain(t, store, "run-epoch", 3)

	// Harden the deployment, then verify: history must survive, and the era
	// split must be visible.
	setHMACKey(t, keyEpochTestKey)

	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: "run-epoch"})
	require.NoError(t, err)
	require.Len(t, traces, 3)

	result := VerifyTraces("run-epoch", traces)
	require.True(t, result.Valid,
		"keying an existing deployment must not invalidate its history")
	assert.Equal(t, 3, result.UnkeyedEraCount,
		"every historical row must be surfaced as pre-key")
	assert.Empty(t, result.Failures)
	_ = v
}
