package state

// Tests for ListAuditChainPage — the paged, ascending, cursor-driven read the
// audit hash chain walks itself with. The chain side is covered in
// internal/audit; what matters here is that BOTH backends answer this query the
// same way, because a difference between them would mean a chain that verifies
// on SQLite and fails on PostgreSQL (or the reverse) with identical data.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeChainRows inserts audit rows in a deliberately unhelpful insertion order:
// timestamps that go backwards, and several rows sharing one instant, so the
// expected order has to come from (timestamp, id) rather than insertion order.
func writeChainRows(t *testing.T, s *SQLiteStore, base time.Time) {
	t.Helper()
	ctx := context.Background()
	rows := []*Audit{
		{ID: "cp-3", Action: "apply", Actor: "alice", Result: "success", Timestamp: base.Add(2 * time.Second)},
		{ID: "cp-1", Action: "login", Actor: "alice", Result: "success", Timestamp: base},
		{ID: "cp-4", Action: "login", Actor: "bob", Result: "success", Timestamp: base}, // ties cp-1
		{ID: "cp-2", Action: "config", Actor: "bob", Result: "success", Timestamp: base.Add(time.Second)},
		{ID: "cp-0", Action: "login", Actor: "carol", Result: "success", Timestamp: base}, // ties cp-1, cp-4
	}
	for _, a := range rows {
		require.NoError(t, s.CreateAudit(ctx, a))
	}
}

func TestSQLiteStore_ListAuditChainPage_AscendingChainOrder(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	writeChainRows(t, s, base)

	page, err := s.ListAuditChainPage(ctx, nil, 10, "")
	require.NoError(t, err)
	ids := make([]string, len(page))
	for i, a := range page {
		ids[i] = a.ID
	}
	assert.Equal(t, []string{"cp-0", "cp-1", "cp-4", "cp-2", "cp-3"}, ids,
		"chain order is (timestamp, id): cp-2 sorts before cp-3, and the three rows sharing a timestamp sort by id")
}

// TestSQLiteStore_ListAuditChainPage_CursorWalksWithoutGapsOrRepeats is the
// paging contract itself: concatenating cursor pages must equal the single-shot
// read, or a seal would compute some rows' hashes against the wrong predecessor.
func TestSQLiteStore_ListAuditChainPage_CursorWalksWithoutGapsOrRepeats(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	writeChainRows(t, s, time.Now().UTC().Truncate(time.Second))

	whole, err := s.ListAuditChainPage(ctx, nil, 100, "")
	require.NoError(t, err)

	var paged []string
	var cursor *AuditCursor
	for {
		page, err := s.ListAuditChainPage(ctx, cursor, 2, "")
		require.NoError(t, err)
		for _, a := range page {
			paged = append(paged, a.ID)
		}
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		cursor = &AuditCursor{Timestamp: last.Timestamp, ID: last.ID}
	}

	var wholeIDs []string
	for _, a := range whole {
		wholeIDs = append(wholeIDs, a.ID)
	}
	assert.Equal(t, wholeIDs, paged, "paging must reproduce the single-shot order exactly")
	assert.Len(t, paged, len(wholeIDs))
}

func TestSQLiteStore_ListAuditChainPage_EmptyLogAndDefaultLimit(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	page, err := s.ListAuditChainPage(ctx, nil, 10, "")
	require.NoError(t, err)
	assert.Empty(t, page, "an empty audit log is not an error")

	// A non-positive limit falls back to the package default rather than
	// returning everything or nothing.
	require.NoError(t, s.CreateAudit(ctx, &Audit{
		ID: "cp-def", Action: "login", Actor: "alice", Result: "success",
		Timestamp: time.Now().UTC(),
	}))
	page, err = s.ListAuditChainPage(ctx, nil, 0, "")
	require.NoError(t, err)
	assert.Len(t, page, 1)

	// A cursor past the end returns nothing rather than wrapping around.
	last := page[0]
	page, err = s.ListAuditChainPage(ctx, &AuditCursor{Timestamp: last.Timestamp.Add(time.Hour), ID: "zzz"}, 10, "")
	require.NoError(t, err)
	assert.Empty(t, page)
}

func TestSQLiteStore_ListAuditChainPage_TenantScope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 3; i++ {
		require.NoError(t, s.CreateAudit(ctx, &Audit{
			ID: "ac-" + string(rune('a'+i)), Action: "login", Actor: "alice",
			Result: "success", TenantID: "acme", Timestamp: base.Add(time.Duration(i) * time.Second),
		}))
		require.NoError(t, s.CreateAudit(ctx, &Audit{
			ID: "gl-" + string(rune('a'+i)), Action: "login", Actor: "bob",
			Result: "success", TenantID: "globex", Timestamp: base.Add(time.Duration(i) * time.Second),
		}))
	}

	page, err := s.ListAuditChainPage(ctx, nil, 10, "acme")
	require.NoError(t, err)
	require.Len(t, page, 3)
	for _, a := range page {
		assert.Equal(t, "acme", a.TenantID)
	}

	// An unscoped page still sees the whole log — the unscoped read is what a
	// single-tenant deployment gets.
	page, err = s.ListAuditChainPage(ctx, nil, 10, "")
	require.NoError(t, err)
	assert.Len(t, page, 6)
}
