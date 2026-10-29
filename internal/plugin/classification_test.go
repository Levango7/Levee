package plugin

// classification_test.go pins the plugin-side half of E-3: the manifest
// reversible_actions list survives the registry round trip (so the plan-time
// gate can read it without executing the plugin), an old registry gains the
// column on open, DeclaredActionClassification reports all three facts the
// dsl policy is built from, and the adapter refuses a manifest that classifies
// one action both ways.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistryPersistsReversibleActions pins Install -> Get -> List for the
// new column, including the absent case (no fabricated declarations).
func TestRegistryPersistsReversibleActions(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	bin := writeTempBinary(t, "binary")

	meta := sampleMeta()
	meta.Type = TypeModule
	meta.Name = "cloud-apply"
	meta.DestructiveActions = []string{"destroy"}
	meta.ReversibleActions = []string{"apply", "status"}

	_, err := r.Install(ctx, meta, bin, "", false)
	require.NoError(t, err)

	got, err := r.Get(ctx, "cloud-apply")
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy"}, got.DestructiveActions)
	assert.Equal(t, []string{"apply", "status"}, got.ReversibleActions)

	records, err := r.List(ctx, "")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, []string{"apply", "status"}, records[0].ReversibleActions)

	plain := sampleMeta()
	plain.Name = "benign"
	plain.Type = TypeModule
	_, err = r.Install(ctx, plain, bin, "", false)
	require.NoError(t, err)
	gotPlain, err := r.Get(ctx, "benign")
	require.NoError(t, err)
	assert.Empty(t, gotPlain.ReversibleActions)
}

// TestRegistryMigratesReversibleActionsColumn targets the immediate previous
// release shape (destructive_actions present, reversible_actions absent): a
// host upgrading in place must gain the column on open, and an old row must
// read as "nothing declared" rather than fail the SELECT.
func TestRegistryMigratesReversibleActionsColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE plugin_registry (
		name TEXT PRIMARY KEY, version TEXT NOT NULL, type TEXT NOT NULL,
		author TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
		entry_point TEXT NOT NULL DEFAULT 'plugin',
		min_host_version TEXT NOT NULL DEFAULT '', max_host_version TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'installed', binary_path TEXT NOT NULL DEFAULT '',
		config_yaml TEXT NOT NULL DEFAULT '', destructive_actions TEXT NOT NULL DEFAULT '[]',
		signature TEXT NOT NULL DEFAULT '',
		installed_at DATETIME NOT NULL DEFAULT (datetime('now')),
		updated_at DATETIME NOT NULL DEFAULT (datetime('now')),
		error_msg TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO plugin_registry
		(name, version, type, state, binary_path, destructive_actions)
		VALUES ('legacy', '1.0.0', 'module', 'enabled', '/tmp/x', '["destroy"]')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	r, err := NewRegistry(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })

	rec, err := r.Get(context.Background(), "legacy")
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy"}, rec.DestructiveActions)
	assert.Empty(t, rec.ReversibleActions, "a pre-classification row declared nothing reversible")

	bin := writeTempBinary(t, "binary")
	m := sampleMeta()
	m.Name = "after-migration"
	m.Type = TypeModule
	m.ReversibleActions = []string{"apply"}
	_, err = r.Install(context.Background(), m, bin, "", false)
	require.NoError(t, err)
	got, err := r.Get(context.Background(), "after-migration")
	require.NoError(t, err)
	assert.Equal(t, []string{"apply"}, got.ReversibleActions)
}

// TestDeclaredActionClassification pins all three outputs: ENABLED MODULE
// records only, "<plugin>.<action>" pairs for both lists, bare module names
// (the fail-closed scope), each sorted.
func TestDeclaredActionClassification(t *testing.T) {
	records := []*RegistryRecord{
		{Name: "zeta", Type: TypeModule, State: StateEnabled,
			DestructiveActions: []string{"destroy"}, ReversibleActions: []string{"status"}},
		{Name: "alpha", Type: TypeModule, State: StateEnabled,
			DestructiveActions: []string{"purge"}, ReversibleActions: []string{"apply"}},
		{Name: "disabled", Type: TypeModule, State: StateDisabled,
			DestructiveActions: []string{"destroy"}, ReversibleActions: []string{"apply"}},
		{Name: "gate", Type: TypeGate, State: StateEnabled,
			DestructiveActions: []string{"destroy"}, ReversibleActions: []string{"apply"}},
		{Name: "benign", Type: TypeModule, State: StateEnabled},
		nil,
	}

	destructive, reversible, modules := DeclaredActionClassification(records)
	assert.Equal(t, []string{"alpha.purge", "zeta.destroy"}, destructive)
	assert.Equal(t, []string{"alpha.apply", "zeta.status"}, reversible)
	assert.Equal(t, []string{"alpha", "benign", "zeta"}, modules)

	// DeclaredDestructivePairs remains the destructive projection.
	assert.Equal(t, destructive, DeclaredDestructivePairs(records))
}

// TestDeclaredReversibleActionsChecksSubsetAndOverlap pins the two refusals:
// an action the handshake does not offer can never run (a phantom exemption),
// and an action in both lists is a contradiction the caller must resolve.
func TestDeclaredReversibleActionsChecksSubsetAndOverlap(t *testing.T) {
	actions := []string{"apply", "destroy"}

	_, err := declaredReversibleActions("cloud", []string{"status"}, actions, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status")
	assert.Contains(t, err.Error(), "does not declare")

	_, err = declaredReversibleActions("cloud", []string{"destroy"}, actions, []string{"destroy"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be both")
}

// TestPluginModuleUnclassifiedActions pins the adapter's view of the
// three-state split: the handshake's actions minus both declarations, sorted,
// and the accessors that expose it.
func TestPluginModuleUnclassifiedActions(t *testing.T) {
	t.Setenv(childEnvKey, "module") // handshake offers apply + destroy

	rec := moduleRecord("cloud-apply")
	rec.ReversibleActions = []string{"apply"}
	m, err := NewPluginModule(context.Background(), rec)
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy"}, m.UnclassifiedActions())
	assert.Equal(t, []string{"apply"}, m.ReversibleActions())
	assert.Empty(t, m.DestructiveActions())

	rec = moduleRecord("cloud-apply")
	rec.DestructiveActions = []string{"destroy"}
	rec.ReversibleActions = []string{"apply"}
	m, err = NewPluginModule(context.Background(), rec)
	require.NoError(t, err)
	assert.Empty(t, m.UnclassifiedActions(), "fully classified plugin leaves no fail-closed default")

	// Both lists naming one action is refused at construction, with the
	// plugin and action in the message.
	rec = moduleRecord("cloud-apply")
	rec.DestructiveActions = []string{"apply"}
	rec.ReversibleActions = []string{"apply"}
	_, err = NewPluginModule(context.Background(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be both")
}

// metaResponse builds the meta answer a module plugin would return, so the
// classifier can be tested without spawning a binary.
func metaResponse(actions ...string) PluginResponse {
	if actions == nil {
		actions = []string{}
	}
	return PluginResponse{OK: true, Result: &ProbeResult{Details: map[string]any{
		"actions":    actions,
		"idempotent": true,
	}}}
}

// moduleRec builds the enabled module record the classifier consumes.
func moduleRec(destructive, reversible []string) *RegistryRecord {
	return &RegistryRecord{
		Name:               "cloud-apply",
		Type:               TypeModule,
		State:              StateEnabled,
		DestructiveActions: destructive,
		ReversibleActions:  reversible,
	}
}

// TestClassifyModuleSurfaceThreeStates pins the exported classifier the CLI
// uses: the same three buckets NewPluginModule computes at boot, from one
// handshake answer plus the installed manifest.
func TestClassifyModuleSurfaceThreeStates(t *testing.T) {
	got, err := ClassifyModuleSurface(
		moduleRec([]string{"destroy"}, []string{"apply"}),
		metaResponse("apply", "destroy", "status"),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"apply", "destroy", "status"}, got.Actions)
	assert.True(t, got.Idempotent)
	assert.Equal(t, []string{"destroy"}, got.Destructive)
	assert.Equal(t, []string{"apply"}, got.Reversible)
	assert.Equal(t, []string{"status"}, got.Unclassified,
		"the verb in neither list is the one the plan-time gate will refuse")
}

// TestClassifyModuleSurfaceFullyClassified is the clean-manifest case: an empty
// Unclassified must be a real answer, not a missing computation.
func TestClassifyModuleSurfaceFullyClassified(t *testing.T) {
	got, err := ClassifyModuleSurface(
		moduleRec([]string{"destroy"}, []string{"apply"}),
		metaResponse("apply", "destroy"),
	)
	require.NoError(t, err)
	assert.Empty(t, got.Unclassified)
}

// TestClassifyModuleSurfaceRefusesPhantomAction pins the shared refusal: an
// action named by the manifest but absent from the handshake cannot run, so it
// must not enter the vocabulary. `plugin test` reports the same refusal serve
// would hit at boot.
func TestClassifyModuleSurfaceRefusesPhantomAction(t *testing.T) {
	_, err := ClassifyModuleSurface(moduleRec([]string{"nuke"}, nil), metaResponse("apply"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nuke")

	_, err = ClassifyModuleSurface(moduleRec(nil, []string{"nuke"}), metaResponse("apply"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nuke")
}

// TestClassifyModuleSurfaceRefusesOverlap pins the contradiction refusal.
func TestClassifyModuleSurfaceRefusesOverlap(t *testing.T) {
	_, err := ClassifyModuleSurface(moduleRec([]string{"apply"}, []string{"apply"}), metaResponse("apply"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be both")
}

// TestClassifyModuleSurfaceRefusesUnusableMeta pins that a handshake which does
// not answer the surface question is an error, not an empty surface: reporting
// "no actions" for a plugin that refused meta would understate the vocabulary.
func TestClassifyModuleSurfaceRefusesUnusableMeta(t *testing.T) {
	_, err := ClassifyModuleSurface(moduleRec(nil, nil), PluginResponse{OK: false, Error: "plugin crashed"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "meta refused")

	_, err = ClassifyModuleSurface(moduleRec(nil, nil), PluginResponse{OK: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no result block")

	_, err = ClassifyModuleSurface(moduleRec(nil, nil), PluginResponse{OK: true, Result: &ProbeResult{Details: map[string]any{}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no actions")
}

// TestClassifyModuleSurfaceNilRecord pins the guard: the classifier is called
// from a command path, where a missing record must fail loudly.
func TestClassifyModuleSurfaceNilRecord(t *testing.T) {
	_, err := ClassifyModuleSurface(nil, metaResponse("apply"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil registry record")
}

// TestClassifyModuleSurfaceMatchesNewPluginModule is the single-source proof:
// the CLI's classifier and the boot path must not be able to disagree, so the
// same inputs must produce the same buckets. The child personality under test
// offers apply + destroy.
func TestClassifyModuleSurfaceMatchesNewPluginModule(t *testing.T) {
	t.Setenv(childEnvKey, "module")

	rec := moduleRec([]string{"destroy"}, nil)
	rec.BinaryPath = os.Args[0]

	m, err := NewPluginModule(context.Background(), rec)
	require.NoError(t, err)

	responses, err := Probe(context.Background(), rec.BinaryPath, []HostRequest{{Action: "meta"}})
	require.NoError(t, err)
	require.Len(t, responses, 1)

	viaCLI, err := ClassifyModuleSurface(rec, responses[0])
	require.NoError(t, err)
	assert.Equal(t, m.Actions(), viaCLI.Actions)
	assert.Equal(t, m.DestructiveActions(), viaCLI.Destructive)
	assert.Equal(t, m.ReversibleActions(), viaCLI.Reversible)
	assert.Equal(t, m.UnclassifiedActions(), viaCLI.Unclassified)
}
