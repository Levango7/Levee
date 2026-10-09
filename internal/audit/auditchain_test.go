package audit

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// These tests cover the audit (not trace) hash chain. The two things they
// exist to pin down are the properties that are easy to believe and hard to
// guarantee:
//
//   - the chain survives a round trip through the store. SQLite persists
//     timestamps at microsecond precision, so a row hashed from the caller's
//     in-memory time.Time would not match the same row read back. Seal hashes
//     what the STORE holds, not what the caller passed, and these tests would
//     fail loudly if that ever changed.
//   - tampering is detected even when the WORM triggers are removed, and the
//     triggers are what stop the tampering in the first place. Testing only
//     one of the two would leave a gap exactly where the design claims
//     defence in depth.

// newAuditChainBuilder builds an AuditChainBuilder over a fresh store.
func newAuditChainBuilder(t *testing.T) (*AuditChainBuilder, *state.SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	b, err := NewAuditChainBuilder(store)
	require.NoError(t, err)
	return b, store
}

// writeAudits inserts n audit rows through Record (the production write path)
// with strictly increasing timestamps, and returns their ids.
func writeAudits(t *testing.T, store state.Store, prefix string, n int, base time.Time) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%d", prefix, i)
		require.NoError(t, Record(ctx, store, &state.Audit{
			ID:        id,
			RunID:     fmt.Sprintf("run-%d", i),
			Action:    "apply",
			Actor:     "alice",
			Target:    fmt.Sprintf("host-%d", i),
			Result:    "success",
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}))
		ids = append(ids, id)
	}
	return ids
}

// dropWORMTriggers removes the audit WORM triggers so a test can simulate a
// DBA who has disabled them, which is the only way to reach the tamper paths
// the chain is supposed to catch.
func dropWORMTriggers(t *testing.T, store *state.SQLiteStore) {
	t.Helper()
	for _, trg := range []string{"worm_prevent_audit_update", "worm_prevent_audit_delete"} {
		_, err := store.DB().ExecContext(context.Background(), "DROP TRIGGER IF EXISTS "+trg)
		require.NoError(t, err, "drop %s", trg)
	}
}

func TestNewAuditChainBuilder_NilStore(t *testing.T) {
	b, err := NewAuditChainBuilder(nil)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.ErrorIs(t, err, ErrNilStore)
}

// TestAuditChain_Record_SealsOnWrite is the load-bearing test for the whole
// feature: the production write path must leave the chain sealed, because a
// chain nobody seals is not evidence of anything.
func TestAuditChain_Record_SealsOnWrite(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	writeAudits(t, store, "aud", 4, time.Now().UTC())

	b, err := NewAuditChainBuilder(store)
	require.NoError(t, err)
	result, err := b.Verify(context.Background())
	require.NoError(t, err)
	assert.True(t, result.Valid, "chain must verify straight after writes: %+v", result.Failures)
	assert.Equal(t, 4, result.Count)
	assert.Equal(t, 0, result.Unsealed)
	assert.Empty(t, result.Failures)
}

// TestAuditChain_SurvivesStoreRoundTrip proves the hash is computed from the
// persisted row rather than the caller's in-memory struct. This is not a
// hypothetical: PostgreSQL's timestamptz keeps microseconds, so a caller
// passing time.Now() hands over nanoseconds the database will never store, and
// a chain built from the caller's value would mismatch on every verification.
// Seal re-reads through the Store, which is what makes it correct on both
// dialects.
func TestAuditChain_SurvivesStoreRoundTrip(t *testing.T) {
	b, store := newAuditChainBuilder(t)
	// A deliberately awkward instant: odd nanoseconds and a non-UTC zone.
	base := time.Date(2024, 3, 17, 8, 5, 3, 123456789, time.FixedZone("CST", 8*3600))
	entry := &state.Audit{
		ID: "rt-0", Action: "login", Actor: "bob", Target: "t",
		Result: "ok", Timestamp: base,
	}
	require.NoError(t, Record(context.Background(), store, entry))

	stored, err := store.GetAudit(context.Background(), "rt-0")
	require.NoError(t, err)
	require.NotNil(t, stored)

	result, err := b.Verify(context.Background())
	require.NoError(t, err)
	require.True(t, result.Valid, "chain must survive the store round trip: %+v", result.Failures)

	// The sealed hash must be the one derived from the row AS STORED. If Seal
	// had hashed the caller's struct instead, this comparison is what would
	// catch it on a dialect that truncates.
	assert.Equal(t, ComputeAuditHash(stored, ""), stored.CurrHash,
		"the chain hash must be derived from the persisted row")
}

// TestAuditChain_OrderIsTotalNotTimestampOnly pins the tie-break. Audit ids are
// random hex, so two rows in the same millisecond have no inherent order; the
// chain must still be reproducible, or Seal and Verify would disagree and the
// chain would be permanently "broken" with no tampering involved.
func TestAuditChain_OrderIsTotalNotTimestampOnly(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	same := time.Now().UTC().Truncate(time.Millisecond)
	// Ids chosen so that insertion order is the REVERSE of id order.
	for _, id := range []string{"aud-c", "aud-a", "aud-b"} {
		require.NoError(t, store.CreateAudit(ctx, &state.Audit{
			ID: id, Action: "login", Actor: "bob", Target: "t",
			Result: "ok", Timestamp: same,
		}))
	}
	b, err := NewAuditChainBuilder(store)
	require.NoError(t, err)
	n, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// A second Seal over the same rows must write nothing: that is only true
	// if the order it derived is stable.
	n2, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "seal must be idempotent; a non-zero count means the order was not stable")

	// The stored order is id order, and each row links to the one before it.
	rows, err := store.ListAudits(ctx, state.AuditFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	byID := map[string]*state.Audit{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	assert.Empty(t, byID["aud-a"].PrevHash, "first in id order starts the chain")
	assert.Equal(t, byID["aud-a"].CurrHash, byID["aud-b"].PrevHash)
	assert.Equal(t, byID["aud-b"].CurrHash, byID["aud-c"].PrevHash)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "%+v", result.Failures)
}

// TestAuditChain_DetectsContentTamper removes the WORM trigger and edits a
// row, which is the scenario the chain exists for: the database-level guard is
// gone and only the hash evidence remains.
func TestAuditChain_DetectsContentTamper(t *testing.T) {
	b, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "tam", 4, time.Now().UTC())

	before, err := b.Verify(ctx)
	require.NoError(t, err)
	require.True(t, before.Valid)

	dropWORMTriggers(t, store)
	_, err = store.DB().ExecContext(ctx,
		`UPDATE audit SET result = 'rolled_back' WHERE id = 'tam-2'`)
	require.NoError(t, err)

	after, err := b.Verify(ctx)
	require.NoError(t, err)
	require.False(t, after.Valid, "a rewritten result must invalidate the chain")
	require.Len(t, after.Failures, 1, "exactly one tampered row must be reported, not a cascade: %+v", after.Failures)

	f := after.Failures[0]
	assert.Equal(t, "tam-2", f.AuditID)
	assert.Equal(t, 2, f.Index)
	assert.Equal(t, FailureHashMismatch, f.Type)
	assert.NotEqual(t, f.Expected, f.Actual)
}

// TestAuditChain_TamperDoesNotCascade checks the cursor advances on the stored
// hash. If it advanced on the recomputed one, a single edited row would make
// every row behind it look broken and the real break would be lost in the
// noise.
func TestAuditChain_TamperDoesNotCascade(t *testing.T) {
	b, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "casc", 5, time.Now().UTC())

	dropWORMTriggers(t, store)
	_, err := store.DB().ExecContext(ctx, `UPDATE audit SET actor = 'mallory' WHERE id = 'casc-1'`)
	require.NoError(t, err)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	require.False(t, result.Valid)
	require.Len(t, result.Failures, 1,
		"only the edited row may be reported; rows behind it are still linked correctly")
	assert.Equal(t, "casc-1", result.Failures[0].AuditID)
}

// TestAuditChain_DetectsDeletion covers the case the chain cannot catch on its
// own — which is why the WORM triggers exist. With them removed, deleting a
// middle row leaves the following row pointing at a hash nothing produced any
// more.
func TestAuditChain_DetectsDeletion(t *testing.T) {
	b, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "del", 4, time.Now().UTC())

	dropWORMTriggers(t, store)
	res, err := store.DB().ExecContext(ctx, `DELETE FROM audit WHERE id = 'del-1'`)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	require.False(t, result.Valid, "a removed row must break the chain")
	require.NotEmpty(t, result.Failures)
	assert.Equal(t, FailurePrevHashMismatch, result.Failures[0].Type,
		"the row after the hole is the one whose predecessor vanished")
	assert.Equal(t, "del-2", result.Failures[0].AuditID)
}

// TestAuditChain_DetectsUnsealedRow: a row that was never sealed is not
// evidence, and must not be reported as valid just because nothing is known
// about it.
func TestAuditChain_DetectsUnsealedRow(t *testing.T) {
	b, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "uns", 2, time.Now().UTC())

	// Write straight to the store, bypassing Record's seal.
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "uns-raw", Action: "config", Actor: "root", Target: "cfg",
		Result: "ok", Timestamp: time.Now().UTC().Add(time.Hour),
	}))

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	require.False(t, result.Valid)
	assert.Equal(t, 1, result.Unsealed)
	require.Len(t, result.Failures, 1)
	assert.Equal(t, FailureEmptyHash, result.Failures[0].Type)
	assert.Equal(t, "uns-raw", result.Failures[0].AuditID)

	// Sealing afterwards repairs exactly the unsealed tail.
	sealed, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, sealed)
	after, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, after.Valid, "%+v", after.Failures)
	assert.Equal(t, 0, after.Unsealed)
}

// TestWORM_BlocksContentUpdate / Delete: the database-level guard, which does
// not depend on any application code running.
func TestWORM_BlocksContentUpdate(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "wrm", 2, time.Now().UTC())

	for _, tc := range []struct{ name, sql string }{
		{"result", `UPDATE audit SET result = 'x' WHERE id = 'wrm-0'`},
		{"actor", `UPDATE audit SET actor = 'x' WHERE id = 'wrm-0'`},
		{"target", `UPDATE audit SET target = 'x' WHERE id = 'wrm-0'`},
		{"timestamp", `UPDATE audit SET timestamp = '2030-01-01 00:00:00' WHERE id = 'wrm-0'`},
		// tenant_id must be immutable too, or one UPDATE moves the record
		// into another tenant's chain without touching any other column.
		{"tenant_id", `UPDATE audit SET tenant_id = 'other' WHERE id = 'wrm-0'`},
		{"run_id", `UPDATE audit SET run_id = 'other' WHERE id = 'wrm-0'`},
		{"id", `UPDATE audit SET id = 'other' WHERE id = 'wrm-0'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.DB().ExecContext(ctx, tc.sql)
			require.Error(t, err, "updating %s must be refused", tc.name)
			assert.Contains(t, err.Error(), "WORM")
		})
	}

	// The row survived untouched.
	got, err := store.GetAudit(ctx, "wrm-0")
	require.NoError(t, err)
	assert.Equal(t, "success", got.Result)
	assert.Empty(t, got.TenantID)
}

func TestWORM_BlocksDelete(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	writeAudits(t, store, "del", 2, time.Now().UTC())

	_, err := store.DB().ExecContext(ctx, `DELETE FROM audit WHERE id = 'del-0'`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM")

	rows, err := store.ListAudits(ctx, state.AuditFilter{})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "the row must still be there")
}

// TestWORM_AllowsChainStamping: the triggers must let the ONE legitimate
// update through, or the chain could never be written at all.
func TestWORM_AllowsChainStamping(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "stamp", Action: "login", Actor: "a", Target: "t",
		Result: "ok", Timestamp: time.Now().UTC(),
	}))

	require.NoError(t, store.UpdateAuditChain(ctx, "stamp", "prev", "curr"))
	got, err := store.GetAudit(ctx, "stamp")
	require.NoError(t, err)
	assert.Equal(t, "prev", got.PrevHash)
	assert.Equal(t, "curr", got.CurrHash)

	// Re-stamping is allowed too: Seal legitimately rewrites a row whose
	// stored hash does not match the recomputed one.
	require.NoError(t, store.UpdateAuditChain(ctx, "stamp", "prev2", "curr2"))
}

func TestUpdateAuditChain_UnknownIDFails(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	err := store.UpdateAuditChain(context.Background(), "nope", "a", "b")
	require.Error(t, err, "a silent no-op would leave an unchained hole in the chain")
	assert.Contains(t, err.Error(), "not found")
}

// TestAuditChain_ConcurrentSealIsSafe exercises the idempotence claim: the
// design says overlapping seals derive identical hashes, so they cannot fork
// the chain. This is the assertion that would fail if Seal ever became
// order-dependent.
func TestAuditChain_ConcurrentSealIsSafe(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	ctx := context.Background()
	// Seed unsealed rows so every goroutine has real work to do.
	base := time.Now().UTC()
	for i := 0; i < 25; i++ {
		require.NoError(t, store.CreateAudit(ctx, &state.Audit{
			ID: fmt.Sprintf("cc-%02d", i), Action: "apply", Actor: "alice",
			Target: fmt.Sprintf("host-%02d", i), Result: "ok",
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}))
	}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, err := NewAuditChainBuilder(store)
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = b.Seal(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}

	b, err := NewAuditChainBuilder(store)
	require.NoError(t, err)
	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "concurrent seals must not fork the chain: %+v", result.Failures)
	assert.Equal(t, 25, result.Count)
}

// TestComputeAuditHash_DistinguishesEveryField: the hash must cover every
// hashed column. A field left out of the canonical encoding is a field an
// attacker can change without tripping the chain.
func TestComputeAuditHash_DistinguishesEveryField(t *testing.T) {
	base := time.Date(2024, 1, 2, 3, 4, 5, 6, time.UTC)
	ref := &state.Audit{
		ID: "a", RunID: "r", Action: "apply", Actor: "alice",
		Target: "host-1", Result: "success", TenantID: "t1", Timestamp: base,
	}
	refHash := ComputeAuditHash(ref, "prev")

	mutations := map[string]func(*state.Audit){
		"ID":        func(a *state.Audit) { a.ID = "b" },
		"RunID":     func(a *state.Audit) { a.RunID = "other" },
		"Action":    func(a *state.Audit) { a.Action = "rollback" },
		"Actor":     func(a *state.Audit) { a.Actor = "mallory" },
		"Target":    func(a *state.Audit) { a.Target = "host-2" },
		"Result":    func(a *state.Audit) { a.Result = "failed" },
		"TenantID":  func(a *state.Audit) { a.TenantID = "t2" },
		"Timestamp": func(a *state.Audit) { a.Timestamp = base.Add(time.Nanosecond) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mutated := *ref
			mutate(&mutated)
			assert.NotEqual(t, refHash, ComputeAuditHash(&mutated, "prev"),
				"changing %s must change the hash", name)
		})
	}

	t.Run("prevHash", func(t *testing.T) {
		assert.NotEqual(t, refHash, ComputeAuditHash(ref, "other"))
	})

	t.Run("deterministic", func(t *testing.T) {
		assert.Equal(t, refHash, ComputeAuditHash(ref, "prev"))
	})

	t.Run("fieldBoundaryUnambiguous", func(t *testing.T) {
		// The V1 pipe encoding collided here: these two rows produced the
		// same joined string, so a collision-style edit went undetected.
		a := &state.Audit{ID: "x|y", RunID: "z", Timestamp: base}
		b := &state.Audit{ID: "x", RunID: "y|z", Timestamp: base}
		assert.NotEqual(t, ComputeAuditHash(a, ""), ComputeAuditHash(b, ""),
			"a '|' inside a field must not be able to impersonate a field boundary")
	})

	t.Run("nilSafe", func(t *testing.T) {
		assert.NotPanics(t, func() { ComputeAuditHash(nil, "") })
	})
}

// TestRecord_NilInputs: Record is now on the path of ten production call
// sites, so its failure modes must be total, not partial.
func TestRecord_NilInputs(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, Record(ctx, nil, &state.Audit{ID: "x"}), ErrNilStore)

	_, store := newAuditChainBuilder(t)
	require.Error(t, Record(ctx, store, nil))
}

// TestRecord_PropagatesCreateFailure: if the row could not be written, Record
// must say so. A swallowed error would make an audit loss invisible — exactly
// what change_service.recordAudit was rewritten to stop.
func TestRecord_PropagatesCreateFailure(t *testing.T) {
	_, store := newAuditChainBuilder(t)
	// A duplicate primary key is a reliable CreateAudit failure.
	entry := &state.Audit{
		ID: "dup", Action: "login", Actor: "a", Target: "t",
		Result: state.AuditResultSuccess, Timestamp: time.Now().UTC(),
	}
	require.NoError(t, Record(context.Background(), store, entry))
	err := Record(context.Background(), store, entry)
	require.Error(t, err, "the duplicate id must surface to the caller")
}

// TestSeal_EmptyLog: sealing an audit log that has no rows is a no-op, not a
// failure — a fresh deployment writes no audit before its first action.
func TestSeal_EmptyLog(t *testing.T) {
	b, _ := newAuditChainBuilder(t)
	ctx := context.Background()

	n, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid, "an empty chain is vacuously intact")
	assert.Equal(t, 0, result.Count)
}
