// End-to-end tests for `levee plugin` (E-2): install/list/info/remove against
// the file-based registry. `enable` spawns the plugin subprocess over gRPC
// and is out of scope for a fake binary.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writePluginDir builds a minimal plugin package: plugin.yaml + entry binary.
func writePluginDir(t *testing.T, name, pluginType string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "name: " + name + "\nversion: 1.0.0\ntype: " + pluginType +
		"\nentry_point: fake-plugin\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.yaml"),
		[]byte(manifest), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fake-plugin"),
		[]byte("#!/bin/sh\necho hi\n"), 0o755))
	return dir
}

func TestPluginE2E_InstallListInfoRemove(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	dir := writePluginDir(t, "demo-gate", "gate")

	// list starts empty (message or table — just must run clean).
	out := mustRun(t, append([]string{"plugin", "list"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))

	// install (JSON asserts the record shape).
	res, _, err := freshJSON(t, append([]string{"plugin", "install", dir, "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "demo-gate", data["name"])

	// list now shows it; info finds it.
	out = mustRun(t, append([]string{"plugin", "list"}, cfg...)...)
	assert.Contains(t, out, "demo-gate")
	out = mustRun(t, append([]string{"plugin", "info", "demo-gate"}, cfg...)...)
	assert.Contains(t, out, "demo-gate")

	// Human install + quiet variant of a second plugin.
	dir2 := writePluginDir(t, "second-mod", "module")
	human := mustRun(t, append([]string{"plugin", "install", dir2}, cfg...)...)
	assert.Contains(t, human, "second-mod")
	quiet := mustRun(t, append([]string{"--quiet", "plugin", "list"}, cfg...)...)
	assert.Contains(t, quiet, "second-mod")

	// remove drops it; info then fails.
	out = mustRun(t, append([]string{"plugin", "remove", "second-mod"}, cfg...)...)
	assert.NotEmpty(t, trimOut(out))
	err = runErr(t, append([]string{"plugin", "info", "second-mod"}, cfg...)...)
	assert.Error(t, err)
}

func TestPluginE2E_InstallErrors(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	// Ghost path: the installer stats the source first.
	err := runErr(t, append([]string{"plugin", "install",
		filepath.Join(t.TempDir(), "nowhere")}, cfg...)...)
	assert.Contains(t, err.Error(), "stat")

	// Missing name in the manifest.
	noName := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(noName, "plugin.yaml"),
		[]byte("version: 1.0.0\ntype: gate\n"), 0o644))
	err = runErr(t, append([]string{"plugin", "install", noName}, cfg...)...)
	assert.Contains(t, err.Error(), "missing name")

	// Invalid plugin type rejected by manifest validation.
	badType := writePluginDir(t, "bad-type", "not-a-type")
	err = runErr(t, append([]string{"plugin", "install", badType}, cfg...)...)
	assert.Contains(t, err.Error(), "invalid type")

	// info/remove on unknown plugins fail.
	err = runErr(t, append([]string{"plugin", "info", "ghost"}, cfg...)...)
	assert.Error(t, err)
	err = runErr(t, append([]string{"plugin", "remove", "ghost"}, cfg...)...)
	assert.Error(t, err)
	// disable on a never-installed plugin fails too.
	err = runErr(t, append([]string{"plugin", "disable", "ghost"}, cfg...)...)
	assert.Error(t, err)
}
