package calendar

// dialect_test.go pins the dialect surface of the calendar store.
//
// Before this change the package had exactly one SQL shape (SQLite) and no way
// to say otherwise, so `EnsureSchema` applied `DATETIME` to whatever handle it
// got — including a PostgreSQL one, where it fails with `type "datetime" does
// not exist`. The two DDL strings that replace it are the same table written
// twice, which is the failure pattern that bit cluster_nodes: two copies drifting
// until a document means different things depending on which path created it. So
// the load-bearing test here is not "each DDL parses" but "the two define the
// same table".

import (
	"context"
	"database/sql"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestDB opens a bare temp SQLite handle: no calendar schema applied, so a
// test can assert what happens to an untouched database.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "dialect-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// ddlColumns extracts the column names declared in a CREATE TABLE statement.
func ddlColumns(t *testing.T, ddl string) []string {
	t.Helper()
	body := regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS calendar_windows \((.*?)\n\);`).FindStringSubmatch(ddl)
	require.Len(t, body, 2, "the DDL must contain the calendar_windows CREATE TABLE block:\n%s", ddl)
	var cols []string
	for _, line := range strings.Split(body[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		name := strings.Fields(line)[0]
		if name == "CONSTRAINT" || name == "PRIMARY" || name == "UNIQUE" || name == "CHECK" {
			continue
		}
		cols = append(cols, name)
	}
	sort.Strings(cols)
	return cols
}

func ddlIndexes(ddl string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`CREATE INDEX IF NOT EXISTS (\w+)`).FindAllStringSubmatch(ddl, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// TestBothDialectsDefineTheSameTable is the drift guard: identical columns and
// identical indexes, whatever types each dialect needs to express them.
func TestBothDialectsDefineTheSameTable(t *testing.T) {
	sqlite := SchemaDDLFor(DialectSQLite)
	pg := SchemaDDLFor(DialectPostgres)

	assert.Equal(t, ddlColumns(t, sqlite), ddlColumns(t, pg),
		"a column present in one dialect and not the other means a store whose rows "+
			"cannot be read by the other shape")
	assert.Equal(t, ddlIndexes(sqlite), ddlIndexes(pg))
	assert.NotEmpty(t, ddlColumns(t, sqlite), "the extractor must actually find the block, not compare two empties")

	// The type difference this whole change exists for.
	assert.Contains(t, sqlite, "DATETIME", "SQLite keeps its own shape")
	assert.NotContains(t, pg, "DATETIME",
		"PostgreSQL has no DATETIME type — applying the SQLite text here is the original bug")
	assert.Contains(t, pg, "TIMESTAMPTZ", "per internal/state's pgschema.sql convention")
}

// TestDialectIsMandatory: guessing SQLite against a PostgreSQL handle fails at
// CREATE TABLE, and guessing placeholders fails at every query. Both are quieter
// than refusing here.
func TestDialectIsMandatory(t *testing.T) {
	_, err := NewStore(context.Background(), nil, DialectUnknown)
	require.Error(t, err, "a nil handle must not paper over an undeclared dialect")

	db := newTestDB(t)
	_, err = NewStore(context.Background(), db, DialectUnknown)
	require.Error(t, err, "DialectUnknown must be refused, not defaulted")
	assert.Contains(t, strings.ToLower(err.Error()), "dialect", "the refusal has to name the cause: %v", err)

	err = EnsureSchema(context.Background(), db, Dialect(42))
	require.Error(t, err, "an out-of-range dialect is the same bug as an unset one")
}

// TestStoreReportsItsDialect keeps the handle and its dialect from being
// reasoned about separately by a future caller.
func TestStoreReportsItsDialect(t *testing.T) {
	s, err := NewStore(context.Background(), newTestDB(t), DialectSQLite)
	require.NoError(t, err)
	assert.Equal(t, DialectSQLite, s.Dialect())
	assert.Equal(t, "sqlite", s.Dialect().String())
	assert.Equal(t, "postgres", DialectPostgres.String())
	assert.Equal(t, "unknown", DialectUnknown.String())
}

// TestPlaceholdersFollowDialect tests the binding rewrite directly: a mis-numbered
// $n binds the wrong value into a column and still "works", which is the worst
// possible failure for a freeze policy.
func TestPlaceholdersFollowDialect(t *testing.T) {
	sqlite := &Store{dialect: DialectSQLite}
	pg := &Store{dialect: DialectPostgres}

	assert.Equal(t, "?, ?, ?", sqlite.phList(3, 1))
	assert.Equal(t, "$1, $2, $3", pg.phList(3, 1))
	assert.Equal(t, "$9", pg.ph(9))
	assert.Equal(t, "?", sqlite.ph(9))
}

// TestAssertNotFrozenNamesTheWindows: the comment on AssertNotFrozen used to
// promise the offending windows and deliver only the target list. Enforcement
// that cannot be acted on becomes noise, so the message is the assertion.
func TestAssertNotFrozenNamesTheWindows(t *testing.T) {
	svc := NewCalendarService(newTestStore(t))
	ctx := context.Background()
	now := mustParseTime(t, "2026-10-01T00:00:00Z")

	require.NoError(t, svc.CreateWindow(ctx, &Window{
		ID: "frz-a", Name: "季度冻结", StartTime: now.Add(-2 * time.Hour), EndTime: now.Add(-time.Hour),
		TargetLabels: []string{"env=prod"}, IsFrozen: true, CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, svc.CreateWindow(ctx, &Window{
		ID: "frz-b", Name: "发布冻结", StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
		TargetLabels: []string{"env=prod"}, IsFrozen: true, CreatedAt: now, UpdatedAt: now,
	}))
	// A non-frozen window over the same labels must not appear in the refusal.
	require.NoError(t, svc.CreateWindow(ctx, &Window{
		ID: "mnt-c", Name: "维护窗口", StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
		TargetLabels: []string{"env=prod"}, IsFrozen: false, CreatedAt: now, UpdatedAt: now,
	}))

	// The wall-clock form must agree that nothing is frozen right now: every
	// window above is anchored to 2026-10-01, which is not "now". Asserting this
	// pair is what keeps the two entry points from drifting apart.
	require.NoError(t, svc.AssertNotFrozen(ctx, []string{"env=prod"}, false),
		"an expired freeze must not block today's change")

	err := svc.AssertNotFrozenAt(ctx, []string{"env=prod"}, now, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFrozen, "callers map the refusal to their transport through this sentinel")
	assert.Contains(t, err.Error(), "frz-b", "the active freeze is named by id")
	assert.Contains(t, err.Error(), "发布冻结", "and by the name the operator gave it")
	assert.Contains(t, err.Error(), "lifts", "and when it ends")
	assert.NotContains(t, err.Error(), "frz-a", "an expired freeze must not be blamed for the refusal")
	assert.NotContains(t, err.Error(), "mnt-c", "a maintenance window is not a freeze")

	assert.NoError(t, svc.AssertNotFrozenAt(ctx, []string{"env=prod"}, now, true),
		"the emergency parameter is the caller's declared override and must bypass")
	assert.NoError(t, svc.AssertNotFrozenAt(ctx, []string{"env=dev"}, now, false),
		"labels outside every freeze are unaffected")
}

// TestFreezeWindowsAtIsTheSameVerdictAsIsFrozen keeps the two functions from
// disagreeing: the refusal message and the boolean must come from one rule.
func TestFreezeWindowsAtIsTheSameVerdictAsIsFrozen(t *testing.T) {
	svc := NewCalendarService(newTestStore(t))
	ctx := context.Background()
	now := mustParseTime(t, "2026-10-01T00:00:00Z")

	require.NoError(t, svc.CreateWindow(ctx, &Window{
		ID: "frz-1", Name: "f1", StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
		TargetLabels: []string{"prod"}, IsFrozen: true, CreatedAt: now, UpdatedAt: now,
	}))

	hits, err := svc.FreezeWindowsAt(ctx, []string{"prod"}, now)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	frozen, err := svc.IsFrozenAt(ctx, []string{"prod"}, now)
	require.NoError(t, err)
	assert.True(t, frozen)

	// Empty label set keeps its documented meaning: any active freeze counts.
	hitsAny, err := svc.FreezeWindowsAt(ctx, nil, now)
	require.NoError(t, err)
	assert.Len(t, hitsAny, 1)
	anyFrozen, err := svc.IsFrozenAt(ctx, nil, now)
	require.NoError(t, err)
	assert.True(t, anyFrozen)
}
