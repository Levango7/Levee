package state

// Migration step-table tests (C-8 prerequisite). The real tables are exercised
// for shape; the step *mechanism* is exercised by temporarily injecting fake
// steps into the package-level tables (restored via t.Cleanup).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationsTable_Shape enforces the convention that both step tables are
// sorted ascending, start at base+1, are gapless, and top out at the current
// version constant. schema.sql/pgschema.sql parity is covered by the v1→v2
// upgrade tests once steps exist.
func TestMigrationsTable_Shape(t *testing.T) {
	check := func(name string, steps []migrationStep, base, current int) {
		t.Helper()
		for i, step := range steps {
			want := base + 1 + i
			assert.Equal(t, want, step.version,
				"%s: step %d must have version %d (sorted, gapless, starts at base+1)", name, i, want)
			assert.NotEmpty(t, step.stmts, "%s: step v%d has no statements", name, step.version)
		}
		if len(steps) > 0 {
			assert.Equal(t, current, steps[len(steps)-1].version,
				"%s: top step version must equal the current schema version", name)
		} else {
			assert.Equal(t, base, current,
				"%s: with no steps, current must equal base", name)
		}
	}
	check("migrations", migrations, baseSchemaVersion, currentSchemaVersion)
	check("pgMigrations", pgMigrations, pgBaseSchemaVersion, pgCurrentSchemaVersion)
}

// withFakeSteps temporarily replaces the SQLite step table.
func withFakeSteps(t *testing.T, steps ...migrationStep) {
	t.Helper()
	orig := migrations
	migrations = steps
	t.Cleanup(func() { migrations = orig })
}

func tableExists(t *testing.T, store *SQLiteStore, table string) bool {
	t.Helper()
	var n int
	err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n)
	require.NoError(t, err)
	return n > 0
}

func TestMigrate_FakeSteps_AppliedIncrementallyAndExactlyOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Fresh store lands on the current version (schema.sql alone).
	v, err := appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	require.Equal(t, currentSchemaVersion, v)

	// Steps are numbered above the real table so they look pending against
	// a database that already carries currentSchemaVersion.
	next := currentSchemaVersion + 1

	// Step appears after the database was built: upgrading runs exactly
	// the pending steps.
	withFakeSteps(t,
		migrationStep{version: next, stmts: []string{`CREATE TABLE fake_step_a (x INTEGER)`}},
	)
	require.NoError(t, Migrate(ctx, store.DB()))
	assert.True(t, tableExists(t, store, "fake_step_a"), "pending step must be applied")
	v, err = appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, next, v, "version row must advance with the step")

	// Re-running with the same table is a no-op (the CREATE would fail if
	// the step re-ran).
	require.NoError(t, Migrate(ctx, store.DB()))

	// A later step skips everything at or below the applied version.
	withFakeSteps(t,
		migrationStep{version: next, stmts: []string{`CREATE TABLE fake_step_a (x INTEGER)`}},
		migrationStep{version: next + 1, stmts: []string{`CREATE TABLE fake_step_b (y INTEGER)`}},
	)
	require.NoError(t, Migrate(ctx, store.DB()))
	assert.True(t, tableExists(t, store, "fake_step_b"))
	v, err = appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, next+1, v)
}

func TestMigrate_FailedStep_IsAtomicAndNotRecorded(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The second statement fails; the first must be rolled back with it and
	// the version must not advance.
	withFakeSteps(t, migrationStep{version: currentSchemaVersion + 1, stmts: []string{
		`CREATE TABLE fake_rolled_back (x INTEGER)`,
		`INSERT INTO no_such_table VALUES (1)`,
	}})
	require.Error(t, Migrate(ctx, store.DB()))
	assert.False(t, tableExists(t, store, "fake_rolled_back"),
		"failed step must roll its whole transaction back")
	v, err := appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v, "failed step must not record its version")

	// Retrying with the fault removed applies the step cleanly.
	withFakeSteps(t, migrationStep{version: currentSchemaVersion + 1, stmts: []string{
		`CREATE TABLE fake_rolled_back (x INTEGER)`,
	}})
	require.NoError(t, Migrate(ctx, store.DB()))
	assert.True(t, tableExists(t, store, "fake_rolled_back"))
}

func TestMigrate_StepsNotReplayedOnFreshDatabase(t *testing.T) {
	// A database built from scratch lands on currentSchemaVersion through
	// schema.sql alone and never replays upgrade steps (by convention
	// schema.sql mirrors them; a replayed ADD COLUMN-style step would fail).
	// An always-failing fake step proves the fresh path skips the table:
	// if it ran, newTestStore's Migrate would error out.
	withFakeSteps(t, migrationStep{version: currentSchemaVersion + 1, stmts: []string{
		`INSERT INTO definitely_missing_table VALUES (1)`,
	}})
	store := newTestStore(t) // Migrate runs at construction and must succeed
	v, err := appliedSchemaVersion(context.Background(), store.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v,
		"fresh build must land on currentSchemaVersion via schema.sql alone")
}

func TestPGMigrate_FakeStep(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	orig := pgMigrations
	pgMigrations = []migrationStep{
		{version: pgCurrentSchemaVersion + 1, stmts: []string{`CREATE TABLE fake_step_pg (x INTEGER)`}},
	}
	t.Cleanup(func() { pgMigrations = orig })

	v, err := pgAppliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	require.Equal(t, pgCurrentSchemaVersion, v)

	require.NoError(t, pgMigrate(ctx, store.DB()))
	var n int
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE tablename = 'fake_step_pg'`).Scan(&n))
	assert.Equal(t, 1, n, "pending pg step must be applied")
	v, err = pgAppliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, pgCurrentSchemaVersion+1, v)

	// Idempotent re-run.
	require.NoError(t, pgMigrate(ctx, store.DB()))
}
