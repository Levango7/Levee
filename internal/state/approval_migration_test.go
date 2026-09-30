// approval_migration_test.go proves the D-1 v2 upgrade is data-safe: a database
// created before the plan_hash/revision columns keeps its existing approval
// rows, and those rows read back with the legacy defaults (empty plan_hash =
// plan-agnostic, revision 0). This is the compatibility guarantee the design
// promises: an in-flight approval chain must survive the schema upgrade.
package state

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrate_LegacyApprovalRowSurvivesUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-approvals.db")
	ctx := context.Background()

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)

	// v1-era schema_version + the tables the replayed steps touch
	// (runs for v3, approvals for v5, credentials for v2), plus the parent
	// row the approval references.
	for _, ddl := range []string{
		`CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT (datetime('now')))`,
		`INSERT INTO schema_version (version) VALUES (1)`,
		`CREATE TABLE runs (
			id TEXT PRIMARY KEY, workflow_name TEXT NOT NULL, template_name TEXT NOT NULL,
			params TEXT NOT NULL DEFAULT '{}', plan_hash TEXT NOT NULL, status TEXT NOT NULL,
			approval_status TEXT NOT NULL DEFAULT 'pending', approval_level TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, creator TEXT NOT NULL,
			incident_id TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE approvals (
			id TEXT PRIMARY KEY, run_id TEXT NOT NULL, level TEXT NOT NULL, approver TEXT NOT NULL,
			status TEXT NOT NULL, comment TEXT NOT NULL DEFAULT '', timeout_at DATETIME,
			acted_at DATETIME, FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE CASCADE)`,
		// v1-era credentials table (for v2 migration that adds tags column)
		`CREATE TABLE credentials (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL,
			encrypted_data BLOB NOT NULL, created_at DATETIME NOT NULL,
			rotated_at DATETIME, UNIQUE (name))`,
		`INSERT INTO runs (id, workflow_name, template_name, plan_hash, status, created_at, updated_at, creator)
			VALUES ('r-legacy', 'wf', 'tpl', 'h', 'draft', '2026-01-01 00:00:00', '2026-01-01 00:00:00', 'alice')`,
		// A pre-upgrade PENDING approval — the in-flight chain that must survive.
		`INSERT INTO approvals (id, run_id, level, approver, status, comment, timeout_at)
			VALUES ('ap-legacy', 'r-legacy', 'high', 'legacy-approver', 'pending', '{}', '2099-01-01 00:00:00')`,
	} {
		_, err := db.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())

	// Opening the store replays the pending steps (v2..v5) over the legacy file.
	store, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	v, err := appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v)

	got, err := store.GetApproval(ctx, "ap-legacy")
	require.NoError(t, err)
	require.NotNil(t, got, "the pre-upgrade approval row must survive")
	assert.Equal(t, "pending", got.Status)
	assert.Empty(t, got.PlanHash, "migrated rows default to the legacy (empty) plan hash")
	assert.Equal(t, int64(0), got.Revision, "migrated rows start at revision 0")

	// The migrated row is still usable: a CAS decision applies.
	got.Status = "approved"
	ok, err := store.UpdateApprovalIfPending(ctx, got)
	require.NoError(t, err)
	require.True(t, ok, "a migrated pending row must still accept a decision")
}
