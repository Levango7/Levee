package state

// SA-018 / SA-019 coverage: credentials.tags CRUD, the real v2 migration on a
// prebuilt v1 database, and the synchronous pragma option.

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func taggedCred(id, name, tags string, now time.Time) *Credential {
	return &Credential{
		ID: id, Name: name, Type: "ssh_key",
		EncryptedData: []byte("ct"), CreatedAt: now, Tags: tags,
	}
}

func TestCredential_TagsRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, store.CreateCredential(ctx,
		taggedCred("c1", "tagged", `{"env":"prod"}`, now)))
	require.NoError(t, store.CreateCredential(ctx,
		taggedCred("c2", "untagged", "", now)))

	got, err := store.GetCredential(ctx, "c1")
	require.NoError(t, err)
	assert.Equal(t, `{"env":"prod"}`, got.Tags)

	byName, err := store.GetCredentialByName(ctx, "tagged")
	require.NoError(t, err)
	assert.Equal(t, `{"env":"prod"}`, byName.Tags)

	got.Tags = `{"env":"staging"}`
	require.NoError(t, store.UpdateCredential(ctx, got))
	after, err := store.GetCredential(ctx, "c1")
	require.NoError(t, err)
	assert.Equal(t, `{"env":"staging"}`, after.Tags)

	list, err := store.ListCredentials(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	byID := map[string]string{}
	for _, c := range list {
		byID[c.ID] = c.Tags
	}
	assert.Equal(t, `{"env":"staging"}`, byID["c1"])
	assert.Equal(t, "", byID["c2"], "tag-less row keeps the zero value")
}

// TestMigrate_V1ToV2_CredentialsTags builds a database that looks exactly
// like one created before SA-018 (v1 credentials DDL without the tags
// column, schema_version row = 1, one legacy row) and verifies the real
// migrations-table step upgrades it in place without losing data.
func TestMigrate_V1ToV2_CredentialsTags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v1.db")
	ctx := context.Background()

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)

	// v1-era DDL: identical to schema.sql minus the tags column.
	_, err = db.ExecContext(ctx, `CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
	)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE credentials (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		type TEXT NOT NULL,
		encrypted_data BLOB NOT NULL,
		created_at DATETIME NOT NULL,
		rotated_at DATETIME,
		UNIQUE (name)
	)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO credentials
		(id, name, type, encrypted_data, created_at)
		VALUES ('old-1', 'prod-ssh', 'ssh_key', x'00', '2026-01-01 00:00:00')`)
	require.NoError(t, err)
	// The v3 step (runs.plan_json) also replays against this legacy file,
	// so the fixture must carry the v1-era runs DDL like any real pre-v2
	// database would.
	_, err = db.ExecContext(ctx, `CREATE TABLE runs (
		id TEXT PRIMARY KEY,
		workflow_name TEXT NOT NULL,
		template_name TEXT NOT NULL,
		params TEXT NOT NULL DEFAULT '{}',
		plan_hash TEXT NOT NULL,
		status TEXT NOT NULL,
		approval_status TEXT NOT NULL DEFAULT 'pending',
		approval_level TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		creator TEXT NOT NULL,
		incident_id TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Opening the store runs the pending v2 step against the legacy file.
	store, err := NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()

	v, err := appliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, currentSchemaVersion, v)

	old, err := store.GetCredential(ctx, "old-1")
	require.NoError(t, err)
	require.NotNil(t, old, "legacy row must survive the upgrade")
	assert.Equal(t, "prod-ssh", old.Name)
	assert.Equal(t, "", old.Tags, "legacy row reads as tag-less")

	// New writes with tags work on the upgraded file.
	require.NoError(t, store.CreateCredential(ctx,
		taggedCred("new-1", "staged", `{"team":"sre"}`, time.Now().UTC())))
	fresh, err := store.GetCredential(ctx, "new-1")
	require.NoError(t, err)
	assert.Equal(t, `{"team":"sre"}`, fresh.Tags)
}

// TestMigrate_FreshAndUpgraded_SchemaShapesMatch guards the sync convention
// between schema.sql and migrations: a fresh database and a v1-upgraded one
// must expose identical column shapes for every migrated table (credentials
// gained tags in v2, runs gained plan_json in v3).
func TestMigrate_FreshAndUpgraded_SchemaShapesMatch(t *testing.T) {
	ctx := context.Background()
	fresh := newTestStore(t)

	legacyPath := filepath.Join(t.TempDir(), "shape-legacy.db")
	db, err := sql.Open("sqlite", legacyPath)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE schema_version (
		version INTEGER PRIMARY KEY,
		applied_at DATETIME NOT NULL DEFAULT (datetime('now')))`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (1)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE TABLE credentials (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL,
		encrypted_data BLOB NOT NULL, created_at DATETIME NOT NULL,
		rotated_at DATETIME, UNIQUE (name))`)
	require.NoError(t, err)
	// v1-era runs DDL (see TestMigrate_V1ToV2_CredentialsTags): the replayed
	// v3 step alters this table.
	_, err = db.ExecContext(ctx, `CREATE TABLE runs (
		id TEXT PRIMARY KEY,
		workflow_name TEXT NOT NULL,
		template_name TEXT NOT NULL,
		params TEXT NOT NULL DEFAULT '{}',
		plan_hash TEXT NOT NULL,
		status TEXT NOT NULL,
		approval_status TEXT NOT NULL DEFAULT 'pending',
		approval_level TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		creator TEXT NOT NULL,
		incident_id TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	upgraded, err := NewSQLiteStore(ctx, legacyPath)
	require.NoError(t, err)
	defer func() { _ = upgraded.Close() }()

	cols := func(store *SQLiteStore, table string) [][3]string {
		rows, err := store.DB().QueryContext(ctx,
			`SELECT name, type, "notnull" || '|' || COALESCE(dflt_value, '<none>')
			 FROM pragma_table_info(?) ORDER BY cid`, table)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out [][3]string
		for rows.Next() {
			var n, ty, rest string
			require.NoError(t, rows.Scan(&n, &ty, &rest))
			out = append(out, [3]string{n, ty, rest})
		}
		require.NoError(t, rows.Err())
		return out
	}
	for _, table := range []string{"credentials", "runs"} {
		assert.Equal(t, cols(fresh, table), cols(upgraded, table),
			"schema.sql and the migration steps must produce the same %s shape", table)
	}
}

func TestNewSQLiteStore_SynchronousOption(t *testing.T) {
	ctx := context.Background()

	query := func(mode string, opts ...SQLiteOption) int {
		store, err := NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "syn.db"), opts...)
		require.NoError(t, err)
		defer func() { _ = store.Close() }()
		var n int
		require.NoError(t, store.DB().QueryRowContext(ctx, "PRAGMA synchronous").Scan(&n))
		return n
	}

	assert.Equal(t, 1, query("default"), "default stays NORMAL (1)")
	assert.Equal(t, 1, query("normal", WithSynchronous("normal")))
	assert.Equal(t, 1, query("empty", WithSynchronous("")))
	assert.Equal(t, 2, query("full", WithSynchronous("full")))
	assert.Equal(t, 2, query("case-insensitive", WithSynchronous(" FULL ")))

	_, err := NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "bad.db"), WithSynchronous("yes-please"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "normal|full")
}

func TestPGMigrate_V1ToV2_CredentialsTags(t *testing.T) {
	store, cleanup := newPGTestStore(t)
	defer cleanup()
	ctx := context.Background()

	// Re-create the v1 shape on the shared test database: drop the column and
	// rewrite the version ledger as exactly {1} (applied = MAX over rows).
	// Leaving the ledger empty would read back applied=0 and send pgMigrate
	// down the fresh-build path (CREATE IF NOT EXISTS no-ops), skipping step
	// replay entirely. IF EXISTS keeps the simulation robust when a previous
	// run left the column already gone. One statement per Exec: the extended
	// protocol rejects multi-command.
	_, err := store.DB().ExecContext(ctx, `ALTER TABLE credentials DROP COLUMN IF EXISTS tags`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `ALTER TABLE runs DROP COLUMN IF EXISTS plan_json`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `DELETE FROM schema_version`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (1)`)
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, `INSERT INTO credentials
		(id, name, type, encrypted_data, created_at)
		VALUES ('old-1', 'prod-ssh', 'ssh_key', '\x00'::bytea, NOW())`)
	require.NoError(t, err)

	v, err := pgAppliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	require.Equal(t, 1, v)

	require.NoError(t, pgMigrate(ctx, store.DB()))
	v, err = pgAppliedSchemaVersion(ctx, store.DB())
	require.NoError(t, err)
	assert.Equal(t, pgCurrentSchemaVersion, v)

	old, err := store.GetCredential(ctx, "old-1")
	require.NoError(t, err)
	require.NotNil(t, old)
	assert.Equal(t, "", old.Tags, "legacy pg row reads back with the default ''")

	require.NoError(t, store.CreateCredential(ctx,
		taggedCred("new-1", "staged", `{"team":"sre"}`, time.Now().UTC())))
	fresh, err := store.GetCredential(ctx, "new-1")
	require.NoError(t, err)
	assert.Equal(t, `{"team":"sre"}`, fresh.Tags)
}
