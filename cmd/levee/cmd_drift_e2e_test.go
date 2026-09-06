// End-to-end tests for `levee drift` (E-2): drive every subcommand RunE
// against the temp-store cliEnv. The prober/snapshot source are real local
// filesystem implementations, so temp files under test control ARE the
// injection seam — no clock/rand seams exist and none are needed (report
// cutoffs tolerate wall-clock time).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeBaselineFile drops a one-check baseline YAML (file check pointed at
// contentPath) and returns its path.
func writeBaselineFile(t *testing.T, contentPath, expected string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "baseline.yaml")
	content := "- check_name: cfg\n  type: file\n  path: '" + filepath.ToSlash(contentPath) +
		"'\n  expected_value: \"" + expected + "\"\n"
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func TestDriftE2E_BaselineSetListShowDelete(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hash123"), 0o644))
	baselineFile := writeBaselineFile(t, contentPath, "hash123")

	out := mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--file", baselineFile}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	out = mustRun(t, append([]string{"drift", "baseline", "list"}, cfg...)...)
	assert.Contains(t, out, "web-01")

	out = mustRun(t, append([]string{"drift", "baseline", "show", "--host", "web-01"}, cfg...)...)
	assert.Contains(t, out, "web-01")

	// JSON shapes.
	res, _, err := freshJSON(t, append([]string{"drift", "baseline", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)
	res, _, err = freshJSON(t, append([]string{"drift", "baseline", "show", "--host", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	mustRun(t, append([]string{"drift", "baseline", "delete", "--host", "web-01"}, cfg...)...)

	// After deletion show must fail with the not-found marker.
	err = runErr(t, append([]string{"drift", "baseline", "show", "--host", "web-01"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")

	// set with a bad baseline file (empty items) errors.
	empty := filepath.Join(t.TempDir(), "empty.yaml")
	require.NoError(t, os.WriteFile(empty, []byte("items: []"), 0o644))
	err = runErr(t, append([]string{"drift", "baseline", "set", "--host", "h2",
		"--file", empty}, cfg...)...)
	assert.Error(t, err)
}

func TestDriftE2E_DetectCleanAndDrifted(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	contentPath := filepath.Join(t.TempDir(), "nginx.conf")
	require.NoError(t, os.WriteFile(contentPath, []byte("hash123"), 0o644))
	baselineFile := writeBaselineFile(t, contentPath, "hash123")
	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "web-01",
		"--file", baselineFile}, cfg...)...)

	// Clean detect (auto baseline, no drift) exits 0.
	out := mustRun(t, append([]string{"drift", "detect", "--host", "web-01"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// Mutate the file: drift IS reported but the command swallows
	// ErrDriftDetected (exit 0 by design).
	require.NoError(t, os.WriteFile(contentPath, []byte("tampered"), 0o644))
	out = mustRun(t, append([]string{"drift", "detect", "--hosts", "web-01"}, cfg...)...)
	assert.Contains(t, out, "web-01")

	// JSON detect on the drifted host.
	res, raw, err := freshJSON(t, append([]string{"drift", "detect", "--host", "web-01", "--json"}, cfg...)...)
	require.NoError(t, err, "detect with drift still exits 0 (swallowed ErrDriftDetected): %s", raw)
	require.NotNil(t, res)

	// Ad-hoc baseline file path (not "auto").
	out = mustRun(t, append([]string{"drift", "detect", "--host", "web-01",
		"--baseline", baselineFile}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// No host at all: usage error exit=2.
	err = runErr(t, append([]string{"drift", "detect"}, cfg...)...)
	assert.Contains(t, err.Error(), "[exit=2]")

	// Unknown host baseline in auto mode: not-found error.
	err = runErr(t, append([]string{"drift", "detect", "--host", "ghost"}, cfg...)...)
	assert.Error(t, err)
}

func TestDriftE2E_BaselineAutoFromSnapshot(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	contentPath := filepath.Join(t.TempDir(), "issue.txt")
	require.NoError(t, os.WriteFile(contentPath, []byte("golden"), 0o644))

	// baseline auto reads <dataDir>/drift/snapshots/<run>/<host>.json.
	snapDir := filepath.Join(e.dataDir, "drift", "snapshots", "run-snap1")
	require.NoError(t, os.MkdirAll(snapDir, 0o755))
	items := []map[string]any{{
		"check_name": "issue", "type": "file",
		"path": filepath.ToSlash(contentPath), "expected_value": "golden",
	}}
	raw, err := json.Marshal(items)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(snapDir, "web-01.json"), raw, 0o644))

	mustRun(t, append([]string{"drift", "baseline", "auto", "--host", "web-01",
		"--run", "run-snap1"}, cfg...)...)

	out := mustRun(t, append([]string{"drift", "detect", "--host", "web-01"}, cfg...)...)
	assert.Contains(t, out, "web-01")

	// Missing snapshot file for the run: error.
	err = runErr(t, append([]string{"drift", "baseline", "auto", "--host", "web-01",
		"--run", "run-missing"}, cfg...)...)
	assert.Error(t, err)
}

func TestDriftE2E_ScheduleLifecycle(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	contentPath := filepath.Join(t.TempDir(), "pkg.txt")
	require.NoError(t, os.WriteFile(contentPath, []byte("v1"), 0o644))
	mustRun(t, append([]string{"drift", "baseline", "set", "--host", "h1",
		"--file", writeBaselineFile(t, contentPath, "v1")}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"drift", "schedule", "add",
		"--name", "nightly", "--cron", "0 2 * * *", "--hosts", "h1",
		"--alert=false", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	jobID, ok := data["id"].(string)
	require.True(t, ok, "schedule add JSON must expose id: %v", res)

	out := mustRun(t, append([]string{"drift", "schedule", "list"}, cfg...)...)
	assert.Contains(t, out, "nightly")

	// Human add variant (covers the non-JSON print path) and quiet variant.
	human := mustRun(t, append([]string{"drift", "schedule", "add", "--name", "weekly",
		"--cron", "0 3 * * 1", "--hosts", "h1"}, cfg...)...)
	assert.Contains(t, human, "Scheduled job added")

	// Explicit run of the (enabled) weekly job probes the file check.
	out = mustRun(t, append([]string{"drift", "schedule", "run", jobID}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// remove JSON variant covers the data shape; human variant the message.
	res, _, err = freshJSON(t, append([]string{"drift", "schedule", "remove", jobID, "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	err = runErr(t, append([]string{"drift", "schedule", "remove", "job-ghost"}, cfg...)...)
	assert.Contains(t, err.Error(), "not found")

	// Invalid cron rejected.
	err = runErr(t, append([]string{"drift", "schedule", "add", "--name", "bad",
		"--cron", "not-a-cron", "--hosts", "h1"}, cfg...)...)
	assert.Error(t, err)

	// Duplicate name may still be allowed; at minimum list stays readable after
	// the lifecycle.
	mustRun(t, append([]string{"drift", "schedule", "list"}, cfg...)...)
}

func TestDriftE2E_Report(t *testing.T) {
	defer resetRootFlags()
	defer resetDriftFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	out := mustRun(t, append([]string{"drift", "report", "--host", "h1", "--days", "7"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	res, _, err := freshJSON(t, append([]string{"drift", "report", "--host", "h1", "--json"}, cfg...)...)
	require.NoError(t, err)
	require.NotNil(t, res)

	// report requires --host.
	err = runErr(t, append([]string{"drift", "report"}, cfg...)...)
	assert.Error(t, err)
}
