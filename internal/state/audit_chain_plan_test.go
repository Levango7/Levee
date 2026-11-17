package state

// audit_chain_plan_test.go pins the SHAPE of the paged chain read, not just
// its results.
//
// Why the shape matters: the audit hash chain walks the whole audit table in
// pages of 1000 rows, using a keyset cursor. The keyset predicate used to be
// spelled as
//
//	(timestamp > ? OR (timestamp = ? AND id > ?))
//
// which SQLite cannot use as an index range seek — it satisfied the predicate
// as a FILTER over a full index scan, and because it was a full scan, EVERY
// page restarted at the leftmost key. The walk was therefore quadratic in the
// number of rows.
//
// Measured on a 100k-row chain (2026-11-17): per-page cost grew from 2 ms on
// the first page to 36 ms on the last, and the whole walk took 1.94 s — 19.4
// us/row against a digest that costs about 1 us/row. The symptom that reached
// the benchmark report was "the per-row cost grows with chain size, cause not
// established"; the cause was this query shape.
//
// The predicate is now a row-value comparison, (timestamp, id) > (?, ?), which
// SQLite CAN use as a range seek: the same walk takes 235 ms (2.35 us/row).
//
// A results-only test cannot catch a regression here: the OR form and the
// row-value form return the SAME rows, just at a different price. So these
// tests assert the plan.
//
// The tests run against the query the production code actually builds
// (auditChainPageSQL / pgAuditChainPageSQL) rather than a hand-written copy —
// otherwise the guard could keep passing against SQL nobody executes.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqlitePlan runs EXPLAIN QUERY PLAN for a query and returns the plan lines
// joined, so a test can assert on the plan's shape.
func sqlitePlan(t *testing.T, s *SQLiteStore, q string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var lines []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		lines = append(lines, detail)
	}
	require.NoError(t, rows.Err())
	return strings.Join(lines, " | ")
}

// TestSQLiteStore_AuditChainPage_KeysetIsAnIndexSeek pins that the keyset
// page read is a range seek on the chain index.
//
// A "SCAN audit" here means: every page re-reads the index from the beginning,
// which is what made the walk quadratic. The plan wording is SQLite's own and
// has been stable across 3.x; the assertion is on the SUBSTANCE (a seek on
// idx_audit_chain), not on incidental formatting.
func TestSQLiteStore_AuditChainPage_KeysetIsAnIndexSeek(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	writeChainRows(t, s, base)

	q, args := auditChainPageSQL(&AuditCursor{Timestamp: base, ID: "cp-1"}, 10, "")
	plan := sqlitePlan(t, s, q, args...)

	assert.Contains(t, plan, "idx_audit_chain",
		"the keyset page read must use the chain index; plan was: %s", plan)
	assert.Contains(t, plan, "SEARCH",
		"the keyset page read must be a range seek, not a scan; plan was: %s", plan)
	assert.NotContains(t, plan, "SCAN audit",
		"a full scan per page is the quadratic shape this test exists to catch; plan was: %s", plan)
}

// TestSQLiteStore_AuditChainPage_RowValueEqualsOrForm pins that the row-value
// predicate answers exactly what the OR predicate answered.
//
// The two are equivalent by SQLite's own rules (row values compare
// lexicographically, column by column, under the same affinity rules), but
// "equivalent by the rules" is a claim about a specification, and this
// project does not take claims on trust: the two forms are run against the
// same rows and their outputs compared, page by page.
func TestSQLiteStore_AuditChainPage_RowValueEqualsOrForm(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UTC().Truncate(time.Second)
	writeChainRows(t, s, base)

	orQuery := `SELECT id, run_id, action, actor, target, result, timestamp, tenant_id, prev_hash, curr_hash FROM audit
		WHERE (timestamp > ? OR (timestamp = ? AND id > ?)) ORDER BY timestamp ASC, id ASC LIMIT ?`
	orFirstPageQuery := `SELECT id, run_id, action, actor, target, result, timestamp, tenant_id, prev_hash, curr_hash FROM audit
		ORDER BY timestamp ASC, id ASC LIMIT ?`

	var cursor *AuditCursor
	for page := 0; page < 5; page++ {
		// Production query.
		q, args := auditChainPageSQL(cursor, 2, "")
		got := queryAuditIDs(t, s, q, args...)

		// The old OR spelling, same cursor, same page size.
		var (
			orQ    = orFirstPageQuery
			orArgs []any
		)
		if cursor != nil {
			orQ = orQuery
			orArgs = append(orArgs, cursor.Timestamp, cursor.Timestamp, cursor.ID)
		}
		orArgs = append(orArgs, 2)
		want := queryAuditIDs(t, s, orQ, orArgs...)

		assert.Equal(t, want, got, "page %d: the row-value predicate must answer what the OR predicate answered", page)
		if len(got) < 2 {
			break
		}
		last := got[len(got)-1]
		ts, id, err := auditCursorOf(t, s, last)
		require.NoError(t, err)
		cursor = &AuditCursor{Timestamp: ts, ID: id}
	}
}

// queryAuditIDs runs a query and returns the ids in order.
func queryAuditIDs(t *testing.T, s *SQLiteStore, q string, args ...any) []string {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, q, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var (
			id, runID, action, actor, target, result, ts, tenant, prev, curr string
		)
		require.NoError(t, rows.Scan(&id, &runID, &action, &actor, &target, &result, &ts, &tenant, &prev, &curr))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// auditCursorOf reads one row's (timestamp, id) so the test can build the next
// cursor exactly the way the store does.
func auditCursorOf(t *testing.T, s *SQLiteStore, id string) (time.Time, string, error) {
	t.Helper()
	ctx := context.Background()
	var ts time.Time
	var gotID string
	err := s.db.QueryRowContext(ctx, "SELECT timestamp, id FROM audit WHERE id = ?", id).Scan(&ts, &gotID)
	return ts, gotID, err
}

// TestPGStore_AuditChainPage_KeysetIsAnIndexSeek pins the same property on the
// PostgreSQL side: the keyset page read must not degrade into a sequential
// scan of the whole audit table.
//
// PostgreSQL's planner could already use the index for each arm of the old OR
// form, so this was never quadratic there — but "never quadratic" is a claim
// about a planner, and planners change. The two backends reading the chain
// differently is exactly the kind of drift this project keeps guards against.
//
// The table is seeded and ANALYZEd first, on purpose: a planner handed an
// empty table may legitimately answer "a sequential scan is fine, there is
// nothing in it", and a guard that depends on an empty table's plan shape
// would be measuring the fixture, not the query.
func TestPGStore_AuditChainPage_KeysetIsAnIndexSeek(t *testing.T) {
	if os.Getenv("LEVEE_PG_TEST_DSN") == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL chain plan test")
	}
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Microsecond)
	const rows = 2000
	for i := 0; i < rows; i++ {
		require.NoError(t, store.CreateAudit(ctx, &Audit{
			ID:        fmt.Sprintf("pgplan-%06d", i),
			Action:    "apply",
			Actor:     "bench",
			Result:    AuditResultSuccess,
			Timestamp: base.Add(time.Duration(i) * time.Millisecond),
		}))
	}
	_, err := store.DB().ExecContext(ctx, "ANALYZE audit")
	require.NoError(t, err)

	q, args := pgAuditChainPageSQL(&AuditCursor{
		Timestamp: base.Add(rows / 2 * time.Millisecond),
		ID:        "pgplan-001000",
	}, 10, "")
	planRows, err := store.DB().QueryContext(ctx, "EXPLAIN "+q, args...)
	require.NoError(t, err)
	defer func() { _ = planRows.Close() }()
	var plan strings.Builder
	for planRows.Next() {
		var line string
		require.NoError(t, planRows.Scan(&line))
		plan.WriteString(line)
		plan.WriteString(" | ")
	}
	require.NoError(t, planRows.Err())

	got := plan.String()
	assert.Contains(t, got, "idx_audit_chain",
		"the keyset page read must use the chain index; plan was: %s", got)
	assert.NotContains(t, got, "Seq Scan on audit",
		"a sequential scan per page is the shape this test exists to catch; plan was: %s", got)
}

// TestAuditChainPageSQL_CursorBindsTwoArgs guards the argument bookkeeping.
//
// The OR form bound the timestamp TWICE (once per arm); the row-value form
// binds it once plus the id. A mismatch between the placeholder count and the
// argument list is a runtime error, but only on the page that has a cursor —
// which is every page but the first, so a broken build could still look fine
// on a single-page chain.
func TestAuditChainPageSQL_CursorBindsTwoArgs(t *testing.T) {
	q, args := auditChainPageSQL(&AuditCursor{Timestamp: time.Now().UTC(), ID: "id-1"}, 7, "acme")
	assert.Contains(t, q, "(timestamp, id) > (?, ?)")
	assert.Equal(t, 4, len(args), "cursor timestamp + cursor id + tenant + limit")
	assert.Contains(t, q, "tenant_id = ?")
	assert.Contains(t, q, "LIMIT ?")

	q, args = auditChainPageSQL(nil, 0, "")
	assert.NotContains(t, q, "timestamp, id", "no cursor means no keyset predicate")
	assert.Equal(t, 1, len(args), "only the limit")
	assert.Equal(t, DefaultAuditChainPageSize, args[0],
		"a non-positive limit falls back to the default page size")
}
