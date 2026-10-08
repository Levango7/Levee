package state_test

// The audit WORM triggers and the global audit hash chain were only exercised
// against SQLite (internal/audit's auditchain_test.go). The PostgreSQL plpgsql
// triggers shipped in schema v6 had zero coverage — nothing on the PG path
// would notice a dropped trigger or a drifted column list. This file pins the
// real trigger behavior against a live PostgreSQL, mirroring
// TestPGStore_TraceHashChainAndWORM for the audit table.
//
// It lives in package state_test (not state) because building the chain needs
// internal/audit, which imports state — an internal test package would create
// an import cycle. It runs only when LEVEE_PG_TEST_DSN is set; the CI postgres
// job runs ./internal/state/... with that variable configured.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

func TestPGStore_AuditChainSealAndWORM(t *testing.T) {
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL audit chain test")
	}
	ctx := context.Background()
	store, err := state.NewPGStore(ctx, dsn, state.PGPoolConfig{
		MaxOpenConns: 5,
		MaxIdleConns: 2,
	})
	require.NoError(t, err)
	defer store.Close()

	for _, tbl := range []string{"audit", "runs", "targets"} {
		_, err := store.DB().ExecContext(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE")
		require.NoError(t, err, tbl)
	}

	base := time.Now().UTC().Truncate(time.Microsecond)

	// Two of the three rows are run-less — exactly the login/config/credential
	// kind a per-run chain structurally cannot reach, so the global chain must
	// seal them too. The shared timestamp with opposite-sorting ids exercises
	// the (timestamp, id) tie-breaker on real PG.
	audits := []*state.Audit{
		{ID: "pg-az-zz", Action: "login", Actor: "alice", Result: "success",
			Timestamp: base.Add(time.Second)},
		{ID: "pg-az-aa", Action: "config", Actor: "bob", Target: "server", Result: "success",
			Timestamp: base.Add(time.Second)},
		{ID: "pg-az-run", RunID: "pg-az-run-1", Action: "apply", Actor: "system",
			Target: "host:h1", Result: "success", Timestamp: base},
	}
	for _, a := range audits {
		require.NoError(t, store.CreateAudit(ctx, a))
	}

	b, err := audit.NewAuditChainBuilder(store)
	require.NoError(t, err)

	// Sealing fills every hash column. Those UPDATEs pass through the WORM
	// trigger's allow-path: only chain columns change, so it must not reject.
	sealed, err := b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, sealed, "all rows are unsealed on the first seal")

	// Sealing is idempotent — a second pass over sealed rows changes nothing.
	sealed, err = b.Seal(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, sealed)

	result, err := b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid)
	assert.Equal(t, 3, result.Count)
	assert.Equal(t, 0, result.Unsealed)
	assert.Empty(t, result.Failures)

	// Every content column is immutable once chained — including tenant_id
	// (or one UPDATE would move an audit row into another tenant) and
	// timestamp (it participates in the chain order).
	for name, stmt := range map[string]string{
		"action":    `UPDATE audit SET action = 'TAMPERED' WHERE id = 'pg-az-zz'`,
		"actor":     `UPDATE audit SET actor = 'mallory' WHERE id = 'pg-az-zz'`,
		"result":    `UPDATE audit SET result = 'failure' WHERE id = 'pg-az-zz'`,
		"target":    `UPDATE audit SET target = 'host:evil' WHERE id = 'pg-az-run'`,
		"run_id":    `UPDATE audit SET run_id = 'pg-az-other' WHERE id = 'pg-az-run'`,
		"tenant_id": `UPDATE audit SET tenant_id = 'tenant-evil' WHERE id = 'pg-az-zz'`,
		"timestamp": `UPDATE audit SET "timestamp" = "timestamp" + interval '1 hour' WHERE id = 'pg-az-zz'`,
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.Error(t, err, "content column %s must be WORM-protected", name)
		assert.Contains(t, err.Error(), "WORM violation", name)
	}

	// Deletion is refused outright.
	_, err = store.DB().ExecContext(ctx, `DELETE FROM audit WHERE id = 'pg-az-zz'`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WORM violation")

	// The refusals actually refused: the row content is what was written.
	var action string
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT action FROM audit WHERE id = 'pg-az-zz'`).Scan(&action))
	assert.Equal(t, "login", action)

	// Chain columns stay writable — that is the seal path — but overwriting one
	// with a value the row's content cannot produce is tampering, and Seal now
	// refuses it: the row no longer satisfies H(content, prev) == curr, which is
	// exactly the signature of an edited row. Rebuild is the deliberate override.
	_, err = store.DB().ExecContext(ctx, `UPDATE audit SET curr_hash = 'evil' WHERE id = 'pg-az-aa'`)
	require.NoError(t, err, "the chain columns must remain updatable")
	result, err = b.Verify(ctx)
	require.NoError(t, err)
	assert.False(t, result.Valid, "a rewritten curr_hash must not verify")

	_, err = b.Seal(ctx)
	require.Error(t, err, "Seal must report a row whose stored hash its content cannot produce")
	assert.ErrorIs(t, err, audit.ErrChainBroken)

	result, err = b.Verify(ctx)
	require.NoError(t, err)
	assert.False(t, result.Valid, "the evidence must survive a seal attempt")

	written, err := b.Rebuild(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, written, "Rebuild rewrites only the row that differs")
	result, err = b.Verify(ctx)
	require.NoError(t, err)
	assert.True(t, result.Valid)
}

// TestPGStore_ListAuditChainPage_OrderCursorAndTenant pins the paged chain read
// on PostgreSQL. It exists because the SQL behind it is not the same SQL the
// SQLite path runs — different placeholder style, and unlike SQLite PostgreSQL
// keeps sub-second timestamps instead of rounding them to whole seconds — so a
// chain that walks correctly on one backend can still be wrong on the other.
// The internal/audit paging tests cover the chain; this covers the query.
func TestPGStore_ListAuditChainPage_OrderCursorAndTenant(t *testing.T) {
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL audit chain page test")
	}
	ctx := context.Background()
	store, err := state.NewPGStore(ctx, dsn, state.PGPoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer store.Close()

	_, err = store.DB().ExecContext(ctx, "TRUNCATE TABLE audit RESTART IDENTITY CASCADE")
	require.NoError(t, err)

	base := time.Now().UTC().Truncate(time.Microsecond)
	// Insertion order is deliberately not chain order; cp-a1/cp-b1/cp-c1 share
	// one instant and must come back ordered by id.
	for _, a := range []*state.Audit{
		{ID: "cp-2", Action: "apply", Actor: "alice", Result: "success", Timestamp: base.Add(time.Second)},
		{ID: "cp-b1", Action: "login", Actor: "alice", Result: "success", Timestamp: base},
		{ID: "cp-1", Action: "config", Actor: "alice", Result: "success", Timestamp: base.Add(500 * time.Millisecond)},
		{ID: "cp-a1", Action: "login", Actor: "alice", Result: "success", Timestamp: base},
		{ID: "cp-c1", Action: "login", Actor: "alice", Result: "success", Timestamp: base},
	} {
		require.NoError(t, store.CreateAudit(ctx, a))
	}

	whole, err := store.ListAuditChainPage(ctx, nil, 100, "")
	require.NoError(t, err)
	wholeIDs := make([]string, len(whole))
	for i, a := range whole {
		wholeIDs[i] = a.ID
	}
	assert.Equal(t, []string{"cp-a1", "cp-b1", "cp-c1", "cp-1", "cp-2"}, wholeIDs,
		"chain order is (timestamp, id) — sub-second precision kept, ties broken by id")

	// Paging by cursor reproduces the single-shot order exactly.
	var paged []string
	var cursor *state.AuditCursor
	for {
		page, err := store.ListAuditChainPage(ctx, cursor, 2, "")
		require.NoError(t, err)
		for _, a := range page {
			paged = append(paged, a.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &state.AuditCursor{Timestamp: last.Timestamp, ID: last.ID}
	}
	assert.Equal(t, wholeIDs, paged)

	// A row appended after the walk was under way belongs to the NEXT page, not
	// to this walk's past — the property keyset paging buys over OFFSET. The
	// cursor still sits on cp-1, so the continuation page carries cp-2 (never
	// read) followed by the newly appended row.
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "cp-3", Action: "login", Actor: "alice", Result: "success",
		TenantID: "tenant-b", Timestamp: base.Add(2 * time.Second),
	}))
	page, err := store.ListAuditChainPage(ctx, cursor, 10, "")
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, "cp-2", page[0].ID)
	assert.Equal(t, "cp-3", page[1].ID, "the row appended mid-walk must be reachable, not skipped")

	page, err = store.ListAuditChainPage(ctx, nil, 10, "tenant-b")
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "cp-3", page[0].ID, "a tenant page must not see other tenants' rows")
}
