package calendar

// pg_dialect_test.go runs the store against a live PostgreSQL.
//
// The dialect abstraction is exactly the kind of change that passes every SQLite
// test while being broken in production: `$n` placeholders mis-number silently
// (a value binds into the wrong column and the query still succeeds), and the
// PostgreSQL DDL is a different string that nothing on the SQLite path ever
// parses. So this file creates the table with the real server, then exercises
// every rewritten query — insert, filter-by-name, filter-by-frozen, active-only,
// limit, update (the re-numbered SET clause plus its WHERE argument) and delete.
//
// Each test runs in its own generated schema with a pinned search_path on a
// single connection: the shared test database is used concurrently by other
// packages and runs, and an unqualified CREATE TABLE IF NOT EXISTS in `public`
// would race them.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgCalendarDB opens the shared test PostgreSQL (LEVEE_PG_TEST_DSN) inside a
// private schema and returns a handle whose one connection sees that schema
// first. Skips the test when no DSN is configured.
func pgCalendarDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping the PostgreSQL calendar test")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	// The search_path below is per-connection, so the pool must stay at one.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, db.PingContext(ctx))

	schema := fmt.Sprintf("calendar_test_%d", time.Now().UnixNano())
	_, err = db.ExecContext(ctx, "CREATE SCHEMA "+schema) // generated name, never user input
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "SET search_path TO "+schema+", public")
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = db.ExecContext(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	return db
}

// TestPostgresSchemaAppliesAndRoundTrips is the regression test for the finding
// that the calendar DDL cannot be created on PostgreSQL at all (`DATETIME` is not
// a PostgreSQL type). It also pins that the freeze verdict survives the dialect:
// reading an is_frozen integer back and comparing TIMESTAMPTZ bounds against now.
func TestPostgresSchemaAppliesAndRoundTrips(t *testing.T) {
	ctx := context.Background()
	db := pgCalendarDB(t)

	store, err := NewStore(ctx, db, DialectPostgres)
	require.NoError(t, err, "the PostgreSQL DDL must apply on a real server")
	assert.Equal(t, DialectPostgres, store.Dialect())
	require.NoError(t, EnsureSchema(ctx, db, DialectPostgres), "applying the schema twice must be a no-op")

	svc := NewCalendarService(store)
	now := time.Now().UTC().Truncate(time.Second)

	frozen := &Window{
		ID: "frz-pg", Name: "pg-freeze",
		StartTime: now.Add(-2 * time.Hour), EndTime: now.Add(2 * time.Hour),
		TargetLabels: []string{"env=prod", "group=web"}, IsFrozen: true,
		RepeatRule: "monthly", CronExpr: "0 0 1 * *",
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, svc.CreateWindow(ctx, frozen))

	maint := &Window{
		ID: "mnt-pg", Name: "pg-window",
		StartTime: now.Add(24 * time.Hour), EndTime: now.Add(26 * time.Hour),
		TargetLabels: []string{"env=staging"}, IsFrozen: false,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, svc.CreateWindow(ctx, maint))

	// GetWindow: WHERE id = $1.
	got, err := svc.GetWindow(ctx, "frz-pg")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "pg-freeze", got.Name)
	assert.True(t, got.IsFrozen, "is_frozen must survive the INTEGER round trip")
	assert.Equal(t, []string{"env=prod", "group=web"}, got.TargetLabels,
		"the JSON label array is written and read back through the same column")
	assert.Equal(t, frozen.StartTime.UTC().Truncate(time.Second), got.StartTime.UTC().Truncate(time.Second),
		"a TIMESTAMPTZ column must not shift the instant")
	assert.Equal(t, frozen.EndTime.UTC().Truncate(time.Second), got.EndTime.UTC().Truncate(time.Second))

	// ListWindows with every filter branch: name ($1), is_frozen ($2),
	// start<=($3), end>($4), LIMIT ($5).
	active, err := svc.ListWindows(ctx, WindowFilter{IsFrozen: ptrBool(true), OnlyActive: true, Now: now})
	require.NoError(t, err)
	require.Len(t, active, 1, "only the freeze covers `now`")
	assert.Equal(t, "frz-pg", active[0].ID)

	byName, err := svc.ListWindows(ctx, WindowFilter{Name: "pg-window"})
	require.NoError(t, err)
	require.Len(t, byName, 1)
	assert.Equal(t, "mnt-pg", byName[0].ID,
		"a mis-numbered $n would filter on is_frozen here and still return a row")

	limited, err := svc.ListWindows(ctx, WindowFilter{Limit: 1})
	require.NoError(t, err)
	assert.Len(t, limited, 1, "LIMIT binds as a parameter on PostgreSQL, not as literal SQL")

	// The freeze verdict an operator cares about, on the real server.
	require.Error(t, svc.AssertNotFrozenAt(ctx, []string{"env=prod"}, now, false),
		"the freeze window covers env=prod, so this must refuse")

	// UpdateWindow: SET clauses $1..$8 with WHERE id = $9. Getting that order
	// wrong rewrites the wrong column and still reports one row affected, which
	// is why the assertions below check columns the update did NOT touch.
	frozen.TargetLabels = []string{"env=prod"}
	frozen.IsFrozen = false
	frozen.UpdatedAt = now.Add(time.Hour)
	require.NoError(t, svc.UpdateWindow(ctx, frozen))

	after, err := svc.GetWindow(ctx, "frz-pg")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.False(t, after.IsFrozen, "the update changed the flag")
	assert.Equal(t, []string{"env=prod"}, after.TargetLabels, "and the labels")
	assert.Equal(t, "0 0 1 * *", after.CronExpr, "cron_expr is NOT in the SET list and must be untouched")
	assert.Equal(t, "pg-freeze", after.Name)

	// Update of a missing id must report not-found (RowsAffected on $9 binding).
	missing := &Window{ID: "nope", Name: "x", StartTime: now, EndTime: now.Add(time.Hour),
		TargetLabels: []string{"a=b"}, CreatedAt: now, UpdatedAt: now}
	require.Error(t, svc.UpdateWindow(ctx, missing))

	require.NoError(t, svc.DeleteWindow(ctx, "mnt-pg"))
	gone, err := svc.GetWindow(ctx, "mnt-pg")
	require.NoError(t, err)
	assert.Nil(t, gone, "the delete's WHERE id binds the right argument")
	require.NoError(t, svc.DeleteWindow(ctx, "mnt-pg"), "deleting a missing window is not an error")
}

// TestSQLiteDialectIsNotAppliedToPostgres: handing the SQLite handle's dialect to
// a PostgreSQL connection is the original bug, so the failure has to be loud.
func TestSQLiteDialectIsNotAppliedToPostgres(t *testing.T) {
	db := pgCalendarDB(t)
	err := EnsureSchema(context.Background(), db, DialectSQLite)
	require.Error(t, err, "the SQLite DDL must fail on PostgreSQL rather than half-create the table")
	assert.Contains(t, err.Error(), "datetime", "the server's own complaint should survive the wrapping: %v", err)
}
