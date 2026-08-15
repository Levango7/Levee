package audit

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordApplyTraces records a realistic sequence of audit traces that simulate
// one full apply run: step execute, gate check, approval, another step, and a
// rollback. It returns the persisted trace ids in chronological order. The
// timestamps are strictly increasing (2ms sleeps between records) so that
// ListTraces returns them in the same order they were recorded.
func recordApplyTraces(t *testing.T, store state.Store, runID string) []string {
	t.Helper()
	ctx := context.Background()
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)

	ids := make([]string, 0, 5)

	// 1. step_execute — deploy via rsync on node-1.
	tr, err := rec.RecordStep(ctx, runID, "host:node-1", "deploy", "rsync",
		map[string]any{"src": "/tmp/build"},
		map[string]any{"bytes": 4096},
		150*time.Millisecond, nil)
	require.NoError(t, err)
	ids = append(ids, tr.ID)
	time.Sleep(2 * time.Millisecond)

	// 2. gate_check — pre-check passes.
	tr, err = rec.RecordGate(ctx, runID, "pre-check", "before", true,
		map[string]any{"checked_hosts": 3})
	require.NoError(t, err)
	ids = append(ids, tr.ID)
	time.Sleep(2 * time.Millisecond)

	// 3. approval_decision — L1 approval by alice.
	tr, err = rec.RecordApproval(ctx, runID, "L1", "alice", "approved", "lgtm")
	require.NoError(t, err)
	ids = append(ids, tr.ID)
	time.Sleep(2 * time.Millisecond)

	// 4. step_execute — restart service on node-2.
	tr, err = rec.RecordStep(ctx, runID, "host:node-2", "restart", "systemctl",
		map[string]any{"unit": "app"},
		map[string]any{"exit_code": 0},
		80*time.Millisecond, nil)
	require.NoError(t, err)
	ids = append(ids, tr.ID)
	time.Sleep(2 * time.Millisecond)

	// 5. rollback_step — roll back the deploy on node-1.
	tr, err = rec.Record(ctx, TraceRecord{
		RunID:    runID,
		Event:    EventRollbackStep,
		Actor:    "system",
		Target:   "host:node-1",
		Input:    map[string]any{"reason": "verification failed"},
		Output:   map[string]any{"restored": true},
		Duration: 30 * time.Millisecond,
		Metadata: map[string]string{"step_name": "deploy", "action": "rollback"},
	})
	require.NoError(t, err)
	ids = append(ids, tr.ID)

	return ids
}

// integrationSetup creates a fresh SQLite store, a run row, and records a full
// apply trace sequence (5 traces). It returns the store and the recorded trace
// ids. This is the common preamble for the end-to-end integration tests.
func integrationSetup(t *testing.T, runID string) (*state.SQLiteStore, []string) {
	t.Helper()
	store := newTestStore(t)
	createRun(t, store, runID)
	ids := recordApplyTraces(t, store, runID)
	return store, ids
}

// hasFailureType reports whether result contains at least one ChainFailure of
// the given FailureType. It helps assert that tampering is detected with the
// expected failure kind without coupling to the exact failure count.
func hasFailureType(result *VerifyResult, ft FailureType) bool {
	for _, f := range result.Failures {
		if f.Type == ft {
			return true
		}
	}
	return false
}

// TestIntegration_FullAuditLoop exercises the complete audit闭环: record →
// build hash chain → verify. A freshly built chain must verify as intact, the
// tail hash must be non-empty, and the tail must equal the last trace's
// CurrHash.
func TestIntegration_FullAuditLoop(t *testing.T) {
	store, ids := integrationSetup(t, "run-full")
	ctx := context.Background()
	require.Len(t, ids, 5)

	// Build the hash chain over the recorded traces.
	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	count, tail, err := builder.Build(ctx, "run-full")
	require.NoError(t, err)
	assert.Equal(t, 5, count)
	assert.NotEmpty(t, tail, "tail hash must be non-empty")

	// Verify the chain integrity — a freshly built chain must pass.
	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-full")
	require.NoError(t, err)
	assert.True(t, result.Valid, "freshly built chain must verify")
	assert.Equal(t, 5, result.Count)
	assert.Empty(t, result.Failures)

	// The tail hash must equal the last trace's CurrHash.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-full"})
	require.NoError(t, err)
	require.Len(t, traces, 5)
	assert.Equal(t, tail, traces[len(traces)-1].CurrHash,
		"tail hash must equal last trace CurrHash")
}

// TestIntegration_TamperDetection_Detail verifies that modifying a trace's
// Detail after the chain is built is detected as a FailureHashMismatch.
func TestIntegration_TamperDetection_Detail(t *testing.T) {
	store, _ := integrationSetup(t, "run-tamper-detail")
	ctx := context.Background()

	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, "run-tamper-detail")
	require.NoError(t, err)

	// Tamper with the middle trace's Detail via raw SQL (bypassing WORM trigger).
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tamper-detail"})
	require.NoError(t, err)
	require.Len(t, traces, 5)
	mid := len(traces) / 2
	originalDetail := traces[mid].Detail
	tamperTraceDetail(t, store, traces[mid].ID, `{"tampered":true}`)
	// Reload to verify the tamper took effect.
	tampered, err := store.GetTrace(ctx, traces[mid].ID)
	require.NoError(t, err)
	require.NotEqual(t, originalDetail, tampered.Detail)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-tamper-detail")
	require.NoError(t, err)
	assert.False(t, result.Valid, "tampered chain must not verify")
	assert.True(t, hasFailureType(result, FailureHashMismatch),
		"must detect FailureHashMismatch after Detail tamper")
}

// TestIntegration_TamperDetection_CurrHash verifies that directly overwriting
// a trace's CurrHash is detected. The last trace is tampered so exactly one
// FailureHashMismatch is reported (no cascading PrevHashMismatch on a next
// record).
func TestIntegration_TamperDetection_CurrHash(t *testing.T) {
	store, _ := integrationSetup(t, "run-tamper-curr")
	ctx := context.Background()

	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, "run-tamper-curr")
	require.NoError(t, err)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tamper-curr"})
	require.NoError(t, err)
	require.Len(t, traces, 5)
	last := len(traces) - 1
	original := traces[last].CurrHash
	tamperTraceCurrHash(t, store, traces[last].ID, "tampered-curr-hash-value")
	// Reload to verify the tamper took effect.
	tampered, err := store.GetTrace(ctx, traces[last].ID)
	require.NoError(t, err)
	require.NotEqual(t, original, tampered.CurrHash)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-tamper-curr")
	require.NoError(t, err)
	assert.False(t, result.Valid, "tampered chain must not verify")
	assert.True(t, hasFailureType(result, FailureHashMismatch),
		"must detect FailureHashMismatch after CurrHash tamper")
}

// TestIntegration_TamperDetection_PrevHash verifies that breaking the chain
// continuity by corrupting a middle trace's PrevHash is detected as a
// FailurePrevHashMismatch.
func TestIntegration_TamperDetection_PrevHash(t *testing.T) {
	store, _ := integrationSetup(t, "run-tamper-prev")
	ctx := context.Background()

	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, "run-tamper-prev")
	require.NoError(t, err)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-tamper-prev"})
	require.NoError(t, err)
	require.Len(t, traces, 5)
	mid := len(traces) / 2
	originalPrev := traces[mid].PrevHash
	tamperTracePrevHash(t, store, traces[mid].ID, "tampered-prev-hash-value")
	// Reload to verify the tamper took effect.
	tampered, err := store.GetTrace(ctx, traces[mid].ID)
	require.NoError(t, err)
	require.NotEqual(t, originalPrev, tampered.PrevHash)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)
	result, err := verifier.Verify(ctx, "run-tamper-prev")
	require.NoError(t, err)
	assert.False(t, result.Valid, "broken chain must not verify")
	assert.True(t, hasFailureType(result, FailurePrevHashMismatch),
		"must detect FailurePrevHashMismatch after PrevHash tamper")
}

// TestIntegration_WORM_AppendOnly exercises the WORM store semantics: Append
// is write-once (duplicate id rejected with ErrAlreadyExists), Read verifies
// the content checksum, and tampering with the stored content makes Read
// return ErrTampered.
func TestIntegration_WORM_AppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createRun(t, store, "run-worm")

	w, err := NewWORMStore(store)
	require.NoError(t, err)

	now := time.Now().UTC()
	trace := newWORMTrace("run-worm", "trace-worm-1", now)

	// Append succeeds and sets the checksum in CurrHash.
	require.NoError(t, w.Append(ctx, trace))
	assert.NotEmpty(t, trace.CurrHash, "Append must set checksum in CurrHash")
	assert.Len(t, trace.CurrHash, 64, "SHA-256 hex checksum is 64 chars")

	// Duplicate id is rejected — WORM forbids overwriting.
	dup := newWORMTrace("run-worm", "trace-worm-1", now.Add(time.Second))
	err = w.Append(ctx, dup)
	require.ErrorIs(t, err, ErrAlreadyExists)

	// Read succeeds and verifies the checksum.
	got, err := w.Read(ctx, "trace-worm-1")
	require.NoError(t, err)
	assert.Equal(t, "trace-worm-1", got.ID)
	assert.Equal(t, "run-worm", got.RunID)

	// Tamper with the Detail via raw SQL (bypassing WORM trigger), simulating
	// an attacker with direct database access. Read must now return ErrTampered
	// because the checksum no longer matches.
	tamperTraceDetail(t, store, "trace-worm-1", `{"tampered":true}`)

	_, err = w.Read(ctx, "trace-worm-1")
	require.ErrorIs(t, err, ErrTampered)
}

// TestIntegration_MultiRun_Independent verifies that two runs have independent
// hash chains: tampering with a trace in run-a does not affect the verification
// of run-b.
func TestIntegration_MultiRun_Independent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createRun(t, store, "run-a")
	createRun(t, store, "run-b")

	recordApplyTraces(t, store, "run-a")
	recordApplyTraces(t, store, "run-b")

	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, "run-a")
	require.NoError(t, err)
	_, _, err = builder.Build(ctx, "run-b")
	require.NoError(t, err)

	verifier, err := NewChainVerifier(store)
	require.NoError(t, err)

	// Both runs verify independently before tampering.
	resA, err := verifier.Verify(ctx, "run-a")
	require.NoError(t, err)
	assert.True(t, resA.Valid, "run-a must verify before tamper")
	resB, err := verifier.Verify(ctx, "run-b")
	require.NoError(t, err)
	assert.True(t, resB.Valid, "run-b must verify before tamper")

	// Tamper with a trace in run-a only (via raw SQL to bypass WORM trigger).
	tracesA, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-a"})
	require.NoError(t, err)
	require.Len(t, tracesA, 5)
	tamperTraceDetail(t, store, tracesA[0].ID, `{"tampered":true}`)

	// run-a must now fail verification.
	resA2, err := verifier.Verify(ctx, "run-a")
	require.NoError(t, err)
	assert.False(t, resA2.Valid, "run-a must fail after tamper")

	// run-b must still verify — runs are independent.
	resB2, err := verifier.Verify(ctx, "run-b")
	require.NoError(t, err)
	assert.True(t, resB2.Valid, "run-b must remain valid after tampering run-a")
}

// TestIntegration_RedactInTrace verifies that sensitive fields (password,
// token) in the Input/Output of a TraceRecord are replaced with [REDACTED] in
// the persisted Detail JSON, so credentials never enter the audit chain.
func TestIntegration_RedactInTrace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createRun(t, store, "run-redact")

	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)

	// Record a trace whose Input carries a password and Output carries a token.
	// The plaintext must never reach the stored Detail.
	trace, err := rec.RecordStep(ctx, "run-redact", "host:node-1", "connect", "ssh",
		map[string]any{
			"cmd":      "ssh deploy@host",
			"password": "super-secret-123",
		},
		map[string]any{
			"stdout": "ok",
			"token":  "bearer-abc",
		},
		50*time.Millisecond, nil)
	require.NoError(t, err)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, redactedValue, d.Input["password"],
		"password must be redacted in stored Detail")
	assert.Equal(t, redactedValue, d.Output["token"],
		"token must be redacted in stored Detail")
	assert.Equal(t, "ssh deploy@host", d.Input["cmd"],
		"non-sensitive input must be preserved")
	assert.Equal(t, "ok", d.Output["stdout"],
		"non-sensitive output must be preserved")

	// The raw Detail JSON must not contain the plaintext credentials.
	assert.NotContains(t, trace.Detail, "super-secret-123")
	assert.NotContains(t, trace.Detail, "bearer-abc")
}

// TestIntegration_ChainOrdering verifies that after Build the traces form a
// proper hash chain in chronological order: the first trace has an empty
// PrevHash, each subsequent trace's PrevHash equals the previous trace's
// CurrHash, and the tail hash equals the last trace's CurrHash.
func TestIntegration_ChainOrdering(t *testing.T) {
	store, _ := integrationSetup(t, "run-order")
	ctx := context.Background()

	builder, err := NewHashChainBuilder(store)
	require.NoError(t, err)
	_, tail, err := builder.Build(ctx, "run-order")
	require.NoError(t, err)

	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-order"})
	require.NoError(t, err)
	require.Len(t, traces, 5)

	// First trace: PrevHash empty (genesis of the chain).
	assert.Empty(t, traces[0].PrevHash, "first trace PrevHash must be empty")
	assert.NotEmpty(t, traces[0].CurrHash, "first trace CurrHash must be non-empty")

	// Each subsequent trace: PrevHash == previous CurrHash (chain continuity).
	for i := 1; i < len(traces); i++ {
		assert.Equal(t, traces[i-1].CurrHash, traces[i].PrevHash,
			"trace %d PrevHash must equal previous CurrHash", i)
		assert.NotEmpty(t, traces[i].CurrHash,
			"trace %d CurrHash must be non-empty", i)
	}

	// Tail hash equals the last trace's CurrHash.
	assert.Equal(t, tail, traces[len(traces)-1].CurrHash,
		"tail hash must equal last trace CurrHash")

	// Timestamps must be non-decreasing (chronological order preserved).
	for i := 1; i < len(traces); i++ {
		assert.True(t, !traces[i].Timestamp.Before(traces[i-1].Timestamp),
			"trace %d timestamp must be >= trace %d timestamp", i, i-1)
	}
}
