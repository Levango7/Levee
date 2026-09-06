// End-to-end tests for `levee target` (E-2): YAML inventory import against
// the cliEnv store, lifecycle status transitions keyed by imported target IDs,
// host history and the channel-less precheck.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func writeInventoryYAML(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "inventory.yaml")
	content := `groups:
  - name: web
  - name: db
targets:
  - address: web-01.example.com
    group: web
    labels:
      role: front
  - address: web-02.example.com
    group: web
  - address: db-01.example.com
    group: db
`
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestTargetE2E_ImportAndLifecycle(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	inv := writeInventoryYAML(t)

	// Import (human + JSON variants).
	out := mustRun(t, append([]string{"target", "import", "--file", inv}, cfg...)...)
	assert.Contains(t, out, "Imported")
	res, _, err := freshJSON(t, append([]string{"target", "import", "--file", inv, "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	// Second import of the same file: everything updates, nothing created.
	assert.Equal(t, float64(0), data["created"])
	assert.Greater(t, data["updated"], float64(0))

	// list: human, quiet, JSON, group/status filters.
	out = mustRun(t, append([]string{"target", "list"}, cfg...)...)
	assert.Contains(t, out, "web-01.example.com")
	quiet := mustRun(t, append([]string{"--quiet", "target", "list"}, cfg...)...)
	assert.Contains(t, quiet, "web-01.example.com")
	out = mustRun(t, append([]string{"target", "list", "--group", "db"}, cfg...)...)
	assert.Contains(t, out, "db-01.example.com")
	assert.NotContains(t, out, "web-01.example.com")
	out = mustRun(t, append([]string{"target", "list", "--status", "active"}, cfg...)...)
	assert.Contains(t, out, "web-02.example.com")
	out = mustRun(t, append([]string{"target", "list", "--format", "text"}, cfg...)...)
	assert.Contains(t, out, "web-01.example.com")

	res, _, err = freshJSON(t, append([]string{"target", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok, "list JSON must be an array of targets: %v", res)
	require.NotEmpty(t, rows)
	first, ok := rows[0].(map[string]any)
	require.True(t, ok)
	targetID, ok := first["id"].(string)
	require.True(t, ok, "target row must expose id: %v", first)

	// Lifecycle: freeze / unfreeze (human), retire (JSON).
	out = mustRun(t, append([]string{"target", "freeze", targetID}, cfg...)...)
	assert.Contains(t, out, "is now frozen")
	out = mustRun(t, append([]string{"target", "unfreeze", targetID}, cfg...)...)
	assert.Contains(t, out, "is now active")
	res, _, err = freshJSON(t, append([]string{"target", "retire", targetID, "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "retired", data["status"])

	// Frozen filter now visible for the retired… (retired, so status
	// filters keep working end to end).
	out = mustRun(t, append([]string{"target", "list", "--status", "retired"}, cfg...)...)
	assert.Contains(t, out, first["hostname"])
}

func TestTargetE2E_ImportErrors(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Missing file.
	err := runErr(t, append([]string{"target", "import", "--file",
		filepath.Join(t.TempDir(), "nope.yaml")}, cfg...)...)
	assert.Contains(t, err.Error(), "read file")

	// Malformed YAML (unknown keys are rejected by KnownFields).
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	require.NoError(t, os.WriteFile(bad, []byte("targets:\n  - address: x\n    bogus: 1\n"), 0o644))
	err = runErr(t, append([]string{"target", "import", "--file", bad}, cfg...)...)
	assert.Error(t, err)

	// Unknown group filter.
	mustRun(t, append([]string{"target", "import", "--file",
		writeInventoryYAML(t), "--default-group", "misc"}, cfg...)...)
	err = runErr(t, append([]string{"target", "list", "--group", "ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")
}

func TestTargetE2E_HistoryAndCheck(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Empty history is a readable empty state, not an error.
	out := mustRun(t, append([]string{"target", "history", "web-99"}, cfg...)...)
	assert.Contains(t, out, "No changes recorded")

	// Seed a run + batch + step for the host (steps carry an FK on
	// batch_id), then history lists the run.
	e.seedRun(t, "run-hist1", "completed")
	store := e.open(t)
	defer func() { _ = store.Close() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, store.CreateBatch(context.Background(), &state.Batch{
		ID: "batch-h1", RunID: "run-hist1", BatchNo: 1, Status: "completed", TotalHosts: 1, Succeeded: 1,
	}))
	require.NoError(t, store.CreateStep(context.Background(), &state.Step{
		ID: "step-h1", RunID: "run-hist1", BatchID: "batch-h1", Host: "web-77",
		StepName: "restart", Status: "ok", StartedAt: &now, CompletedAt: &now,
	}))

	out = mustRun(t, append([]string{"target", "history", "web-77"}, cfg...)...)
	assert.Contains(t, out, "run-hist1")
	res, _, err := freshJSON(t, append([]string{"target", "history", "web-77", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)
	// limit <= 0 falls back to the 20 default.
	mustRun(t, append([]string{"target", "history", "web-77", "--limit", "0"}, cfg...)...)

	// Precheck without a channel reports the host instead of crashing.
	out = mustRun(t, append([]string{"target", "check", "web-01"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))
	res, _, err = freshJSON(t, append([]string{"target", "check", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)
}
