// store_selection_pg_test.go is the live-PostgreSQL half of the claim this
// change exists for: an operator who sets `database.driver: postgres` must get a
// CLI that talks to the server's database, and a freeze written there must be
// the verdict read back from there.
//
// Everything else in store_selection_test.go proves the decision by its failure
// shape (an unreachable PostgreSQL errors instead of quietly creating a local
// file). This file proves the success path, which only a real server can check:
// the PostgreSQL DDL applies, the store reports itself as postgres through the
// generic interface, and the calendar service built on that handle enforces.
//
// It runs in a generated private schema with the search_path pinned per
// connection, because the CI test database is shared with the state, cluster and
// takeover packages — an unqualified create in `public` would race them, and the
// schema is dropped afterwards so nothing is left behind either way.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/calendar"
	"github.com/nexus/levee/internal/state"
)

// pgScopedDSN creates a private schema in the shared test database and returns
// a DSN whose connections land in it. Skips when no test database is configured.
func pgScopedDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LEVEE_PG_TEST_DSN")
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping the live PostgreSQL store-selection test")
	}

	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx), "the configured LEVEE_PG_TEST_DSN must be reachable")

	schema := fmt.Sprintf("cli_store_test_%d", time.Now().UnixNano())
	_, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema) // generated name, never user input
	require.NoError(t, err)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(c, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	// pgx forwards `options` to the backend, so every connection in the pool
	// starts inside the schema; NewPGStore then migrates and the calendar DDL
	// lands there too.
	return dsn + sep + "options=-c%20search_path%3D" + schema
}

func TestPostgresConfigRoundTripsFreezeThroughTheCLIStore(t *testing.T) {
	scoped := pgScopedDSN(t)
	ctx := context.Background()

	cfgPath, dbPath := writeStoreConfig(t, state.DriverPostgres, scoped)
	optConfigPath = cfgPath
	defer resetRootFlags()

	// The whole point of the change, in one line: the store a CLI command opens
	// for this config is a PostgreSQL store, not the operator's local file.
	store, err := openStore(ctx)
	require.NoError(t, err, "openStore failed against the configured PostgreSQL")
	defer func() { _ = store.Close() }()
	require.Equal(t, state.DriverPostgres, state.StoreDriver(store))
	_, statErr := os.Stat(dbPath)
	require.True(t, os.IsNotExist(statErr),
		"a live PostgreSQL run must still not leave a SQLite file behind: %s", dbPath)

	// The calendar must open over that same handle in the PostgreSQL dialect and
	// enforce what was written through it.
	svc, err := calendarFor(ctx, store)
	require.NoError(t, err, "the calendar table must apply on a real PostgreSQL server")

	now := time.Now().UTC().Truncate(time.Second)
	label := fmt.Sprintf("dsn-frozen-%d", now.UnixNano())
	err = svc.CreateWindow(ctx, &calendar.Window{
		ID: "cli-dsn-freeze", Name: "release freeze", TargetLabels: []string{label},
		StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour), IsFrozen: true,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	err = svc.AssertNotFrozen(ctx, []string{label}, false)
	require.Error(t, err, "a freeze written over the PostgreSQL handle was not enforced by it")
	require.True(t, errors.Is(err, calendar.ErrFrozen), "got %v", err)
	// The same handle still honours the author-declared emergency override.
	require.NoError(t, svc.AssertNotFrozen(ctx, []string{label}, true))

	// And a second, independent process-shaped open of the same config sees the
	// same row — which is what makes the CLI's write visible to a server.
	store2, err := openStore(ctx)
	require.NoError(t, err)
	defer func() { _ = store2.Close() }()
	svc2, err := calendarFor(ctx, store2)
	require.NoError(t, err)
	frozen, err := svc2.IsFrozen(ctx, []string{label})
	require.NoError(t, err)
	require.True(t, frozen, "the freeze did not survive a fresh connection to the same database")
}
