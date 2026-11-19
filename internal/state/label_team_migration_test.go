package state

// label_team_migration_test.go — v11 exists, adds exactly the two columns it
// promises, and lands on both a fresh database and an upgraded one.
//
// What the step does NOT do matters as much as what it does: label and team are
// ADDED columns defaulting to ''. Nothing can be recovered from a pre-v11 row
// because the create path never wrote either field, so any other value would
// report attribution nobody gave — the same "do not guess" rule the v9 priority
// split follows. The test pins both properties: default '' and no backfill.

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestV11StepAddsLabelAndTeamOnly(t *testing.T) {
	var found int
	for _, step := range migrations {
		if step.version != 11 {
			continue
		}
		found++
		require.Len(t, step.stmts, 2, "v11 adds exactly two columns")
		assert.Contains(t, step.stmts[0], "ADD COLUMN label")
		assert.Contains(t, step.stmts[1], "ADD COLUMN team")
		for _, stmt := range step.stmts {
			assert.Contains(t, stmt, `DEFAULT ''`,
				"a pre-v11 row has no authored label or team; '' is the only truthful value")
			assert.NotContains(t, stmt, "UPDATE",
				"there is nothing to backfill — the values were never written before v11")
		}
	}
	require.Equal(t, 1, found, "exactly one version-11 step (no duplicates, none missing)")
}

func TestV11LandsOnFreshAndUpgradedDatabases(t *testing.T) {
	ctx := context.Background()

	fresh := newTestStore(t)
	assert.True(t, hasColumns(t, fresh.DB(), "label", "team"), "a fresh database must carry both columns")

	// Simulate a database frozen at v10: neither column exists, the ledger says
	// 10, and a row created there has no label/team to lose.
	upgraded := newTestStore(t)
	db := upgraded.DB()
	_, err := db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN label`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN team`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM schema_version`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (10)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO runs (id, workflow_name, template_name, params, plan_hash, status,
			approval_status, approval_level, created_at, updated_at, creator)
			VALUES ('run-old', 'wf.yaml', 'tpl', '{}', '', 'draft', 'pending', '',
			        CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'alice')`)
	require.NoError(t, err)

	require.NoError(t, Migrate(ctx, db))
	assert.True(t, hasColumns(t, db, "label", "team"), "an upgraded database must gain both columns")

	run, err := upgraded.GetRun(ctx, "run-old")
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Empty(t, run.Label, "a row that predates v11 has no authored label")
	assert.Empty(t, run.Team)
}

// Both schema files must declare the columns: a fresh SQLite database is built
// from schema.sql and a fresh PostgreSQL one from pgschema.sql, and they have to
// agree about what a run carries — a mismatch would make the same binary read
// different shapes depending on a deployment flag.
func TestBothSchemaFilesDeclareLabelAndTeam(t *testing.T) {
	for _, rel := range []string{"schema.sql", "pgschema.sql"} {
		cols := runsDeclaredColumns(t, rel)
		assert.Contains(t, cols, "label", "%s declares no runs.label", rel)
		assert.Contains(t, cols, "team", "%s declares no runs.team", rel)
	}
}

// hasColumns reports whether every named column exists on runs.
func hasColumns(t *testing.T, db *sql.DB, names ...string) bool {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `PRAGMA table_info(runs)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	present := map[string]bool{}
	for rows.Next() {
		var (
			cid      int
			name     string
			typ      string
			notNull  int
			defValue any
			pk       int
		)
		require.NoError(t, rows.Scan(&cid, &name, &typ, &notNull, &defValue, &pk))
		present[name] = true
	}
	require.NoError(t, rows.Err())

	for _, name := range names {
		if !present[name] {
			return false
		}
	}
	return true
}
