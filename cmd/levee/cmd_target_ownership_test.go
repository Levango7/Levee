package main

// cmd_target_ownership_test.go covers the LOCAL half of ADR-008's write-side
// enforcement.
//
// `levee target import` does not go through InventoryService.ImportTargets: it
// opens the store and calls inventory.Importer directly. That is precisely why
// the check has to exist here too — a gate that only guards the gRPC path is a
// gate with a documented bypass, and this command is the one people actually
// run from a terminal.
//
// The data directory the registry is loaded from is cfg.Server.DataDir, the same
// place serve reads it from, so a host registered for the server is registered
// for the CLI.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
)

// writeConflictingInventory creates an inventory file that declares db-1 as dev.
func writeConflictingInventory(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conflict.yaml")
	require.NoError(t, os.WriteFile(p, []byte("targets:\n  - address: db-1\n    labels:\n      env: dev\n"), 0o644))
	return p
}

// writeOwnershipInDataDir places target-ownership.yaml where both serve and the
// CLI look for it (cfg.Server.DataDir). cliEnv.dataDir IS that directory — the
// fixture config sets server.data_dir to it — so the helper does not reload the
// config, which would read the process-wide --config flag instead of this
// test's environment.
func writeOwnershipInDataDir(t *testing.T, e *cliEnv) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(e.dataDir, inventory.OwnershipFileName),
		[]byte("hosts:\n  - hostname: db-1\n    environment: prod\n"), 0o600))
}

// resetTargetImportFlags keeps the package-level import options from leaking
// into the next test: cobra stores the parsed value on the flag, so a test that
// passed --allow-env-conflict would otherwise leave it set for whatever runs
// next in this process.
func resetTargetImportFlags() {
	targetImportOptFile = ""
	targetImportOptGroup = ""
	targetImportOptAllowEnvConflict = false
}

func TestTargetImportLocalRefusesEnvConflictByDefault(t *testing.T) {

	defer resetTargetImportFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	writeOwnershipInDataDir(t, e)

	err := runErr(t, append([]string{"target", "import", "--file", writeConflictingInventory(t)}, cfg...)...)
	require.Error(t, err, "the local path must not be a bypass of the ownership check")
	assert.Contains(t, err.Error(), "allow-env-conflict",
		"the refusal must name the local flag, not the gRPC field")
	assert.Contains(t, err.Error(), "prod")
	assert.Contains(t, err.Error(), "dev")

	// Refused before the write: no row, so a failed import cannot leave the
	// inventory half-applied.
	store := e.open(t)
	defer func() { _ = store.Close() }()
	rows, lerr := store.ListTargets(context.Background(), state.TargetFilter{})
	require.NoError(t, lerr)
	assert.Empty(t, rows)
}

func TestTargetImportLocalAllowEnvConflictWritesAndAudits(t *testing.T) {

	defer resetTargetImportFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	writeOwnershipInDataDir(t, e)

	out := mustRun(t, append([]string{"target", "import", "--file", writeConflictingInventory(t),
		"--allow-env-conflict"}, cfg...)...)
	assert.Contains(t, out, "Imported")

	store := e.open(t)
	defer func() { _ = store.Close() }()

	row, err := store.FindTargetByAddress(context.Background(), "db-1", 22)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, "dev", row.Labels["env"], "the override writes the file as declared")

	entries, err := store.ListAudits(context.Background(), state.AuditFilter{
		Action: state.AuditActionImportEnvConflict,
	})
	require.NoError(t, err)
	require.Len(t, entries, 1, "a local override must leave the same evidence the RPC path does")
	assert.Contains(t, entries[0].Target, "db-1:22")
	assert.NotEmpty(t, entries[0].Actor)
}

func TestTargetImportLocalKeepsWorkingWithNoRegistry(t *testing.T) {
	// The compatibility promise for the CLI: no target-ownership.yaml means the
	// deployment has not opted in, and `target import` behaves exactly as before.

	defer resetTargetImportFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	inv := writeInventoryYAML(t)

	out := mustRun(t, append([]string{"target", "import", "--file", inv}, cfg...)...)
	assert.Contains(t, out, "Imported")
}

func TestTargetImportLocalAcceptsTheRegisteredEnvironment(t *testing.T) {

	defer resetTargetImportFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	writeOwnershipInDataDir(t, e)

	// A consistent declaration is not a conflict and must import silently.
	p := filepath.Join(t.TempDir(), "consistent.yaml")
	require.NoError(t, os.WriteFile(p, []byte("targets:\n  - address: db-1\n    labels:\n      env: prod\n"), 0o644))
	out := mustRun(t, append([]string{"target", "import", "--file", p}, cfg...)...)
	assert.Contains(t, out, "Imported")
	assert.NotContains(t, out, "warning: imported", "a consistent import must not print an override warning")
}
