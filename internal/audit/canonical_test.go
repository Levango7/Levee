package audit

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// --- canonical encoding (fix: length-prefixed fields) -----------------------

// TestCanonicalV2_Unambiguous verifies the reason V2 exists: two different
// field tuples that collapse into the same byte stream under the legacy
// pipe-joined encoding must produce distinct digests under V2.
func TestCanonicalV2_Unambiguous(t *testing.T) {
	a := []string{"run|1", "detail"}
	b := []string{"run", "1|detail"}
	require.Equal(t, canonicalV1(a...), canonicalV1(b...), "precondition: legacy encoding is ambiguous")
	assert.NotEqual(t, canonicalV2(a...), canonicalV2(b...), "V2 encoding must disambiguate field boundaries")
}

// TestComputeHash_V2DiffersFromLegacy pins that ComputeHash uses the V2
// canonical encoding by default.
func TestComputeHash_V2DiffersFromLegacy(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	tr := &state.Trace{ID: "t1", RunID: "r1", Event: "plan", Actor: "alice",
		Detail: `{"k":"v"}`, Timestamp: now}
	got := ComputeHash(tr, "")
	assert.Equal(t, ComputeHashV2(tr, ""), got)
	assert.NotEqual(t, legacyHash(tr, ""), got, "new chains must not use the legacy encoding")
}

// --- legacy chain acceptance (VerifyHashChain against pre-existing data) ----

// seedChainRun creates a run plus n trace records and returns their ids.
func seedChainRun(t *testing.T, store *state.SQLiteStore, runID string, n int) []string {
	t.Helper()
	createRun(t, store, runID)
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)
	var ids []string
	for i := 0; i < n; i++ {
		tr, err := rec.Record(context.Background(), TraceRecord{
			RunID:  runID,
			Event:  EventStepExecute,
			Actor:  "tester",
			Output: map[string]any{"i": i},
		})
		require.NoError(t, err)
		ids = append(ids, tr.ID)
	}
	return ids
}

func storedTraces(t *testing.T, store *state.SQLiteStore, runID string) []*state.Trace {
	t.Helper()
	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	return traces
}

// TestVerify_AcceptsLegacyChain rebuilds a chain using the legacy V1
// encoding and asserts both verifiers accept it as intact.
func TestVerify_AcceptsLegacyChain(t *testing.T) {
	b, store := newChainBuilder(t)
	const runID = "run-legacy"
	seedChainRun(t, store, runID, 3)

	// Overwrite hashes with the legacy pipe-delimited chain, walking the
	// records in the same deterministic order Verify uses
	// (timestamp ASC, id ASC).
	prev := ""
	for _, trace := range storedTraces(t, store, runID) {
		trace.PrevHash = prev
		trace.CurrHash = legacyHash(trace, prev)
		require.NoError(t, store.UpdateTrace(context.Background(), trace))
		prev = trace.CurrHash
	}

	count, err := b.Verify(context.Background(), runID)
	require.NoError(t, err, "legacy chains must still verify after the V2 upgrade")
	assert.Equal(t, 3, count)

	v, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := v.Verify(context.Background(), runID)
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Empty(t, result.Failures)
	assert.Equal(t, 3, result.LegacyCount, "all records matched only the legacy encoding")

	// Tampering is still detected inside a legacy chain.
	traces := storedTraces(t, store, runID)
	tamperTraceDetail(t, store, traces[2].ID, `{"evil":true}`)
	_, verifyErr := b.Verify(context.Background(), runID)
	require.ErrorIs(t, verifyErr, ErrHashMismatch)
}

// TestVerify_DetectsTamperUnderV2 guards against the legacy fallback
// accepting arbitrary garbage: a hash matching neither encoding fails.
func TestVerify_DetectsGarbageHash(t *testing.T) {
	b, store := newChainBuilder(t)
	const runID = "run-v2"
	ids := seedChainRun(t, store, runID, 2)

	n, tail, err := b.Build(context.Background(), runID)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.NotEmpty(t, tail)

	tamperTraceCurrHash(t, store, ids[1], "deadbeef-not-a-real-digest")
	_, err = b.Verify(context.Background(), runID)
	require.ErrorIs(t, err, ErrHashMismatch)
}

// --- WORM checksum: legacy + tamper interplay -------------------------------

func TestWORMStore_ReadAcceptsLegacyChecksum(t *testing.T) {
	worm, store := newWORMStore(t)
	ctx := context.Background()
	createRun(t, store, "run-worm-legacy")

	tr := newWORMTrace("run-worm-legacy", "trace-legacy", time.Now().UTC())
	require.NoError(t, worm.Append(ctx, tr))

	// Rewrite curr_hash to what a pre-V2 deployment would have stored.
	stored, err := store.GetTrace(ctx, tr.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	stored.CurrHash = legacyChecksum(stored)
	require.NoError(t, store.UpdateTrace(ctx, stored))

	got, err := worm.Read(ctx, tr.ID)
	require.NoError(t, err, "legacy WORM checksums must still verify")
	assert.Equal(t, legacyChecksum(got), got.CurrHash)
}

// --- HMAC keying ------------------------------------------------------------

func setHMACKey(t *testing.T, value string) {
	t.Helper()
	old, hadOld := os.LookupEnv(EnvHMACKey)
	require.NoError(t, os.Setenv(EnvHMACKey, value))
	resetKeyForTest()
	t.Cleanup(func() {
		if hadOld {
			_ = os.Setenv(EnvHMACKey, old)
		} else {
			_ = os.Unsetenv(EnvHMACKey)
		}
		resetKeyForTest()
	})
}

// resetKeyForTest re-arms the lazy key resolution. It exists purely for
// tests in this package; production code must never call it.
func resetKeyForTest() {
	keyOnce = sync.Once{}
	hmacKey = nil
}

func unsetHMACKey(t *testing.T) {
	t.Helper()
	setHMACKey(t, "")
	_ = os.Unsetenv(EnvHMACKey)
	resetKeyForTest()
}

func TestHMACKeying_KeyedDigests(t *testing.T) {
	unsetHMACKey(t)
	tr := &state.Trace{ID: "id", RunID: "r", Event: "e", Actor: "a", Detail: "d"}
	unkeyed := ComputeChecksum(tr)

	setHMACKey(t, "0123456789abcdef0123456789abcdef") // >= 16 raw bytes
	require.True(t, Keyed())
	keyed := ComputeChecksum(tr)
	assert.NotEqual(t, unkeyed, keyed, "keyed digest must differ from plain SHA-256")
	assert.Equal(t, keyed, ComputeChecksum(tr), "keyed digest stays deterministic")

	// The hash chain uses the same funnel.
	assert.Equal(t, ComputeHashV2(tr, ""), ComputeHash(tr, ""))
}

func TestHMACKeying_ShortKeyIgnored(t *testing.T) {
	unsetHMACKey(t)
	tr := &state.Trace{ID: "id", RunID: "r", Event: "e", Actor: "a", Detail: "d"}
	unkeyed := ComputeChecksum(tr)

	setHMACKey(t, "short-key") // < 16 bytes -> warn + ignore
	require.False(t, Keyed())
	assert.Equal(t, unkeyed, ComputeChecksum(tr), "weak key must fall back to unkeyed SHA-256")
}

func TestHMACKeying_UnsetUsesSHA256(t *testing.T) {
	unsetHMACKey(t)
	require.False(t, Keyed())
	tr := &state.Trace{ID: "id", RunID: "r", Event: "e", Actor: "a", Detail: "d"}
	sum := ComputeChecksum(tr)
	assert.Len(t, sum, 64, "hex sha256 length")
}

// --- Redact recursion --------------------------------------------------------

func TestRedactStringMap(t *testing.T) {
	in := map[string]string{
		"step_name": "restart",
		"password":  "hunter2",
		"API_KEY":   "abc123",
		"note":      "safe",
	}
	out := RedactStringMap(in)
	assert.Equal(t, "[REDACTED]", out["password"])
	assert.Equal(t, "[REDACTED]", out["API_KEY"])
	assert.Equal(t, "restart", out["step_name"])
	assert.Equal(t, "safe", out["note"])

	// Input untouched.
	assert.Equal(t, "hunter2", in["password"])
	assert.Nil(t, RedactStringMap(nil))
}

func TestRedact_RecursiveIntoSlicesAndStringMaps(t *testing.T) {
	in := map[string]any{
		"batch": []any{
			map[string]any{"host": "h1", "token": "leaky"},
			map[string]string{"secret": "s3cr3t", "step": "one"},
		},
		"metadata": map[string]string{"credential": "top-secret", "phase": "1"},
	}
	out := Redact(in)

	slice, ok := out["batch"].([]any)
	require.True(t, ok, "slice values must stay slices")
	first, ok := slice[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "h1", first["host"])
	assert.Equal(t, redactedValue, first["token"])

	second, ok := slice[1].(map[string]string)
	require.True(t, ok)
	assert.Equal(t, redactedValue, second["secret"])
	assert.Equal(t, "one", second["step"])

	meta, ok := out["metadata"].(map[string]string)
	require.True(t, ok)
	assert.Equal(t, redactedValue, meta["credential"])
	assert.Equal(t, "1", meta["phase"])

	// Original input untouched.
	assert.Equal(t, "leaky", in["batch"].([]any)[0].(map[string]any)["token"])
}
