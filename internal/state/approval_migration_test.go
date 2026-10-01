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

	// A complete v1-era database (see legacy_v1_test.go): opening the store
	// replays every pending step, and v6 alters all ten tenant-owned tables,
	// so a partial fixture cannot exercise this path.
	createLegacyV1DB(t, ctx, db)

	for _, ddl := range []string{
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

	// Opening the store replays the pending steps (v2..v6) over the legacy file.
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
