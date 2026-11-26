package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// TestHMACKeying_KeySwitchKeepsHistoryVerifiable records the D-8 fix.
//
// The hazard this file used to pin: the HMAC key was deployment-global and
// chain records carried no key-epoch marker, so a deployment that started
// keyed could not recompute a chain built unkeyed — every historical row was
// reported as tampered, and the verifier could not distinguish "the operator
// changed the key" from "someone rewrote the chain".
//
// The fix gives the SHA-256 family the same self-describing prefix the 2026-11
// SM3 migration already proved for the algorithm dimension: a keyed row is
// "hmac-sha256:<hex>" and an unkeyed row stays bare 64-hex. Verification reads
// each row's own prefix, so historical rows keep the meaning they were written
// with and no backfill is needed. See canonical.go (prefixHMACSHA256) and
// VerifyResult.UnkeyedEraCount for the operator-facing visibility.
//
// The boundary stays documented rather than pretended away: an actor who can
// write rows could strip the prefix from a keyed row and have it verified as
// unkeyed. That is the same exposure an unkeyed deployment has always had, and
// it is why the pre-keying era is REPORTED (UnkeyedEraCount) instead of being
// silently absorbed — the checklist in docs/deployment.md §5.6/§15 was updated
// in the same change to say so.
func TestHMACKeying_KeySwitchKeepsHistoryVerifiable(t *testing.T) {
	unsetHMACKey(t)
	b, store := newChainBuilder(t)
	ctx := context.Background()
	createRun(t, store, "run-key-switch")
	recordTraces(t, store, "run-key-switch", 4)

	_, _, err := b.Build(ctx, "run-key-switch")
	require.NoError(t, err)

	// Unkeyed: the chain verifies, and every row is in the unkeyed era.
	count, err := b.Verify(ctx, "run-key-switch")
	require.NoError(t, err)
	assert.Equal(t, 4, count)
	require.False(t, Keyed())

	// The operator adds the key to harden an existing deployment. Historical
	// rows must stay intact — this is the whole point of the epoch marker.
	setHMACKey(t, "0123456789abcdef0123456789abcdef")
	require.True(t, Keyed())

	count, err = b.Verify(ctx, "run-key-switch")
	require.NoError(t, err, "keying an existing deployment must not invalidate its history")
	assert.Equal(t, 4, count)

	// The read-only verifier used by the console endpoints agrees, and reports
	// the era split instead of hiding it (one verifier, one answer).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-key-switch"})
	require.NoError(t, err)
	require.Len(t, traces, 4)
	result := VerifyTraces("run-key-switch", traces)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Failures)
	assert.Equal(t, 4, result.UnkeyedEraCount,
		"all four rows predate the key and must be surfaced as such")

	// Note on appending: Build refuses to extend a run that already has hashes
	// (ErrChainBroken) — appending destroys evidence unless an operator forces
	// it, which is pre-existing behaviour and orthogonal to the key epoch. The
	// mixed-era case is therefore exercised directly in
	// TestMixedEraChainVerifiesRowByRow.
}

// TestMixedEraChainVerifiesRowByRow builds a chain that spans both eras by
// hand (four rows written unkeyed, two appended under the key, chained
// through the real writer funnels) and verifies it row by row. This is the
// property the epoch marker exists to provide, and it is asserted without
// depending on HashChainBuilder's append policy.
func TestMixedEraChainVerifiesRowByRow(t *testing.T) {
	useMode(t, digestModeSHA256)

	// Era 1: unkeyed, as a historical deployment wrote them.
	unsetHMACKey(t)
	unkeyedTraces := []*state.Trace{
		{ID: "t1", RunID: "r", Event: "e1", Actor: "a", Detail: "d1"},
		{ID: "t2", RunID: "r", Event: "e2", Actor: "a", Detail: "d2"},
		{ID: "t3", RunID: "r", Event: "e3", Actor: "a", Detail: "d3"},
		{ID: "t4", RunID: "r", Event: "e4", Actor: "a", Detail: "d4"},
	}
	chain(unkeyedTraces)
	prev := unkeyedTraces[len(unkeyedTraces)-1].CurrHash

	// Era 2: the operator keys the deployment, then two more records land.
	setHMACKey(t, keyEpochTestKey)
	keyedTraces := []*state.Trace{
		{ID: "t5", RunID: "r", Event: "e5", Actor: "a", Detail: "d5"},
		{ID: "t6", RunID: "r", Event: "e6", Actor: "a", Detail: "d6"},
	}
	chainFrom(prev, keyedTraces)

	all := append(unkeyedTraces, keyedTraces...)

	// The two eras really do look different on disk.
	require.Len(t, unkeyedTraces[0].CurrHash, 64, "unkeyed rows stay bare")
	assert.True(t, strings.HasPrefix(keyedTraces[0].CurrHash, prefixHMACSHA256),
		"keyed rows carry the prefix")

	result := VerifyTraces("r", all)
	require.True(t, result.Valid, "a chain spanning both eras must verify row by row")
	assert.Empty(t, result.Failures)
	assert.Equal(t, 6, result.Count)
	assert.Equal(t, 4, result.UnkeyedEraCount, "exactly the pre-keying rows are reported")

	// Removing the key again must not retroactively break the unkeyed rows, and
	// must break the keyed ones (they cannot be recomputed without the key).
	unsetHMACKey(t)
	afterKeyRemoved := VerifyTraces("r", all)
	assert.False(t, afterKeyRemoved.Valid,
		"keyed rows become unverifiable once the key is gone")
	assert.Equal(t, 4, afterKeyRemoved.UnkeyedEraCount,
		"the unkeyed rows are still intact and still counted as pre-keying")
}

// chain fills PrevHash/CurrHash for an ordered slice using the same funnel
// the writer uses, so the test exercises production hashing rather than a stub.
func chain(traces []*state.Trace) { chainFrom("", traces) }

// chainFrom continues an existing chain: prev is the previous tail hash, not
// the empty string, so appended records keep continuity with what came before.
func chainFrom(prev string, traces []*state.Trace) {
	for _, tr := range traces {
		tr.PrevHash = prev
		tr.CurrHash = ComputeHashV2(tr, prev)
		prev = tr.CurrHash
	}
}

// TestKeyedRowFailsWhenKeyRemoved is the other half of the marker: a keyed row
// must NOT silently fall back to the unkeyed algorithm, because that would let
// anyone who can strip a prefix forge a verified row.
func TestKeyedRowFailsWhenKeyRemoved(t *testing.T) {
	unsetHMACKey(t)
	useMode(t, digestModeSHA256)
	setHMACKey(t, "0123456789abcdef0123456789abcdef")

	keyed := digestWithMode("payload", digestModeSHA256)
	require.True(t, len(keyed) > 64, "premise: a keyed digest carries a prefix")

	unsetHMACKey(t)
	_, err := recomputeCanonical("payload", keyed)
	require.Error(t, err, "a keyed row must not verify once the key is gone")
	assert.Contains(t, err.Error(), EnvHMACKey)
}

// TestUnknownPrefixFailsClosed keeps the guard the SM3 migration introduced:
// a colon that is not a known prefix is a programming error or tampering, never
// a silent fallback to the historical algorithm.
func TestUnknownPrefixFailsClosed(t *testing.T) {
	unsetHMACKey(t)
	useMode(t, digestModeSHA256)

	_, err := recomputeCanonical("payload", "bogus-family:deadbeef")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown digest prefix")
}

// TestKeyEra classification reads the prefix only, so it never depends on the
// process configuration.
func TestKeyEra(t *testing.T) {
	setHMACKey(t, "0123456789abcdef0123456789abcdef")
	useMode(t, digestModeSHA256)

	assert.Equal(t, eraUnkeyed, keyEra(unkeyedSHA256("p")), "bare hex is the pre-keying era")
	assert.Equal(t, eraKeyed, keyEra(digestWithMode("p", digestModeSHA256)), "keyed sha256 declares itself")
	assert.Equal(t, eraKeyed, keyEra(digestWithMode("p", digestModeSM3)), "keyed sm3 declares itself")
	assert.Equal(t, eraUnkeyed, keyEra(unkeyedSM3ForTest(t)), "unkeyed sm3 declares itself")
	assert.Equal(t, eraUnknown, keyEra(""), "an unbuilt chain is neither era")
	assert.Equal(t, eraUnknown, keyEra("nonsense:hex"), "an unknown prefix is not assumed unkeyed")

	// Classification must not follow the current key state.
	unsetHMACKey(t)
	assert.Equal(t, eraKeyed, keyEra("hmac-sha256:"+unkeyedSHA256("p")),
		"a keyed-era row stays keyed even after the key is removed from the environment")
}

// unkeyedSM3ForTest returns an unkeyed SM3 digest without disturbing the
// caller's key state.
func unkeyedSM3ForTest(t *testing.T) string {
	t.Helper()
	unsetHMACKey(t)
	return digestWithMode("p", digestModeSM3)
}
