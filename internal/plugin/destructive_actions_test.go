package plugin

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryPersistsDestructiveActions pins the round trip that lets the
// plan-time irreversibility gate read a plugin's destructive declaration
// without executing the plugin: the manifest's DestructiveActions must survive
// Install -> Get and Install -> List.
func TestRegistryPersistsDestructiveActions(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	bin := writeTempBinary(t, "binary")

	meta := sampleMeta()
	meta.Type = TypeModule
	meta.Name = "cloud-apply"
	meta.DestructiveActions = []string{"destroy", "purge"}

	_, err := r.Install(ctx, meta, bin, "", false)
	require.NoError(t, err)

	got, err := r.Get(ctx, "cloud-apply")
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy", "purge"}, got.DestructiveActions)

	// A plugin that declares none must read back as nil/empty, not as a
	// fabricated list.
	plain := sampleMeta()
	plain.Name = "benign"
	plain.Type = TypeModule
	_, err = r.Install(ctx, plain, bin, "", false)
	require.NoError(t, err)
	gotPlain, err := r.Get(ctx, "benign")
	require.NoError(t, err)
	assert.Empty(t, gotPlain.DestructiveActions)

	records, err := r.List(ctx, StateEnabled)
	require.NoError(t, err)
	assert.Empty(t, records, "installed records are not yet enabled")
}

// TestEnsurePluginRegistryColumnsMigratesOldTable is the migration guard: a
// registry created by a host from before this column existed must gain it on
// open, or every SELECT naming destructive_actions would fail on an upgraded
// deployment.
func TestEnsurePluginRegistryColumnsMigratesOldTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.db")

	// Recreate the PRE-migration table shape (no destructive_actions) and a
	// row, exactly as an older host would have left it.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE plugin_registry (
		name TEXT PRIMARY KEY, version TEXT NOT NULL, type TEXT NOT NULL,
		author TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
		entry_point TEXT NOT NULL DEFAULT 'plugin',
		min_host_version TEXT NOT NULL DEFAULT '', max_host_version TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'installed', binary_path TEXT NOT NULL DEFAULT '',
		config_yaml TEXT NOT NULL DEFAULT '', signature TEXT NOT NULL DEFAULT '',
		installed_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
		error_msg TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO plugin_registry
		(name, version, type, state, binary_path)
		VALUES ('legacy', '1.0.0', 'module', 'enabled', '/tmp/x')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Opening through NewRegistry must migrate the table in place.
	r, err := NewRegistry(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	rec, err := r.Get(context.Background(), "legacy")
	require.NoError(t, err)
	assert.Equal(t, "legacy", rec.Name)
	assert.Empty(t, rec.DestructiveActions, "an old row has no declaration")

	// And a fresh install on the migrated table still round-trips.
	bin := writeTempBinary(t, "binary")
	m := sampleMeta()
	m.Name = "after-migration"
	m.Type = TypeModule
	m.DestructiveActions = []string{"wipe"}
	_, err = r.Install(context.Background(), m, bin, "", false)
	require.NoError(t, err)
	got, err := r.Get(context.Background(), "after-migration")
	require.NoError(t, err)
	assert.Equal(t, []string{"wipe"}, got.DestructiveActions)
}

// TestDeclaredDestructivePairs pins what the plan-time vocabulary is built
// from: ENABLED MODULE records only (a disabled or non-module plugin cannot run
// a step, so its declaration must not enter the vocabulary), in sorted order so
// the verdict is deterministic regardless of registry row order.
func TestDeclaredDestructivePairs(t *testing.T) {
	records := []*RegistryRecord{
		{Name: "zeta", Type: TypeModule, State: StateEnabled, DestructiveActions: []string{"destroy"}},
		{Name: "alpha", Type: TypeModule, State: StateEnabled, DestructiveActions: []string{"purge", "destroy"}},
		{Name: "disabled", Type: TypeModule, State: StateDisabled, DestructiveActions: []string{"destroy"}},
		{Name: "gate", Type: TypeGate, State: StateEnabled, DestructiveActions: []string{"destroy"}},
		{Name: "benign", Type: TypeModule, State: StateEnabled},
		nil,
	}
	got := DeclaredDestructivePairs(records)
	assert.Equal(t, []string{"alpha.destroy", "alpha.purge", "zeta.destroy"}, got)
}

// TestDeclaredDestructiveActionsRejectsUnknownAction pins the subset check:
// a manifest may only call destructive an action the plugin's meta handshake
// actually offers, otherwise the vocabulary would contain a pair that can never
// run (a phantom, and a governance lie).
func TestDeclaredDestructiveActionsRejectsUnknownAction(t *testing.T) {
	_, err := declaredDestructiveActions("cloud", []string{"destroy"}, []string{"apply"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destroy")
	assert.Contains(t, err.Error(), "does not declare")
}

// TestDeclaredDestructiveActionsDedupsAndSorts pins the deterministic shape:
// manifest order and duplicates must not change the vocabulary, because the
// irreversibility verdict is baked into the plan hash.
func TestDeclaredDestructiveActionsDedupsAndSorts(t *testing.T) {
	got, err := declaredDestructiveActions("cloud",
		[]string{"purge", "destroy", "purge"}, []string{"apply", "destroy", "purge"})
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy", "purge"}, got)

	none, err := declaredDestructiveActions("cloud", nil, []string{"apply"})
	require.NoError(t, err)
	assert.Nil(t, none)
}
