package main

// cmd_plugin_classification_test.go covers the operator-facing half of the E-3
// three-state action classification: what `plugin info` and `plugin list`
// surface from the registry, and what `plugin test` prints once the handshake
// is in. The registry half is exercised end-to-end (a manifest goes through
// install into the record), because the whole point of the feature is that the
// verdict is readable WITHOUT running the plugin.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plugin"
)

// writeModulePluginDir builds a module plugin package whose manifest declares
// both action lists. install never executes the binary, so its content is
// irrelevant — which is exactly the property these tests assert.
func writeModulePluginDir(t *testing.T, name string, destructive, reversible []string) string {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("name: " + name + "\nversion: 1.0.0\ntype: module\nentry_point: fake-plugin\n")
	writeList(&b, "destructive_actions", destructive)
	writeList(&b, "reversible_actions", reversible)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(b.String()), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fake-plugin"), []byte("#!/bin/sh\necho hi\n"), 0o755))
	return dir
}

// writeList emits a YAML inline list, or nothing at all when the list is empty:
// an absent key and an empty key must stay distinguishable, because the
// classification reads "declared nothing" as "everything is unclassified".
func writeList(b *strings.Builder, key string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString(key + ": [" + strings.Join(items, ", ") + "]\n")
}

func TestPluginInfoJSONSurfacesModuleDeclarations(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	dir := writeModulePluginDir(t, "cloud-apply", []string{"destroy"}, []string{"apply", "status"})
	mustRun(t, append([]string{"plugin", "install", dir}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"plugin", "info", "cloud-apply", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, []any{"destroy"}, data["destructive_actions"])
	assert.Equal(t, []any{"apply", "status"}, data["reversible_actions"])
}

// TestPluginInfoOmitsClassificationForNonModule is the control: a gate plugin
// has no actions a step can name, so printing empty action lists would read as
// "declared nothing destructive" instead of "not applicable".
func TestPluginInfoOmitsClassificationForNonModule(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	dir := writePluginDir(t, "demo-gate", "gate")
	mustRun(t, append([]string{"plugin", "install", dir}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"plugin", "info", "demo-gate", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, data, "destructive_actions")
	assert.NotContains(t, data, "reversible_actions")
}

// TestPluginInfoHumanNamesBothListsAndTheDefault pins the human view, including
// the fail-closed sentence an operator needs to act on.
func TestPluginInfoHumanNamesBothListsAndTheDefault(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	dir := writeModulePluginDir(t, "cloud-apply", []string{"destroy"}, nil)
	mustRun(t, append([]string{"plugin", "install", dir}, cfg...)...)

	out := mustRun(t, append([]string{"plugin", "info", "cloud-apply"}, cfg...)...)
	assert.Contains(t, out, "Destructive actions:")
	assert.Contains(t, out, "destroy")
	assert.Contains(t, out, "Reversible actions:\t(none declared)")
	assert.Contains(t, out, "Actions in neither list are judged irreversible at plan time.")
	assert.Contains(t, out, "levee plugin test")
}

// TestPluginRecordToMapModuleOnly asserts the mapping directly, since the JSON
// path goes through encoding and hides which key was absent.
func TestPluginRecordToMapModuleOnly(t *testing.T) {
	mod := pluginRecordToMap(&plugin.RegistryRecord{
		Name:               "cloud-apply",
		Type:               plugin.TypeModule,
		DestructiveActions: []string{"destroy"},
		ReversibleActions:  []string{"apply"},
	})
	assert.Equal(t, []string{"destroy"}, mod["destructive_actions"])
	assert.Equal(t, []string{"apply"}, mod["reversible_actions"])

	gate := pluginRecordToMap(&plugin.RegistryRecord{Name: "demo-gate", Type: plugin.TypeGate})
	assert.NotContains(t, gate, "destructive_actions")
	assert.NotContains(t, gate, "reversible_actions")
}

// TestPrintModuleSurfaceAlwaysNamesUnclassified pins the test-output shape:
// "(none)" is an answer, its absence is not.
func TestPrintModuleSurfaceAlwaysNamesUnclassified(t *testing.T) {
	var buf bytes.Buffer
	printModuleSurface(&buf, plugin.ModuleSurface{
		Actions:      []string{"apply", "destroy"},
		Destructive:  []string{"destroy"},
		Reversible:   []string{"apply"},
		Unclassified: nil,
	})
	got := buf.String()
	assert.Contains(t, got, "destructive: destroy")
	assert.Contains(t, got, "reversible: apply")
	assert.Contains(t, got, "unclassified (judged irreversible at plan time): (none)")
}

func TestPrintModuleSurfaceListsUnclassified(t *testing.T) {
	var buf bytes.Buffer
	printModuleSurface(&buf, plugin.ModuleSurface{
		Actions:      []string{"apply", "probe"},
		Reversible:   []string{"apply"},
		Unclassified: []string{"probe"},
	})
	assert.Contains(t, buf.String(), "unclassified (judged irreversible at plan time): probe")
}

// TestPluginListJSONCarriesModuleDeclarations pins that the roster (not just
// the detail view) carries the declarations, because `plugin list --json` is
// what a deployment audit script reads.
func TestPluginListJSONCarriesModuleDeclarations(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}

	dir := writeModulePluginDir(t, "cloud-apply", []string{"destroy"}, []string{"apply"})
	mustRun(t, append([]string{"plugin", "install", dir}, cfg...)...)

	res, _, err := freshJSON(t, append([]string{"plugin", "list", "--json"}, cfg...)...)
	require.NoError(t, err)
	rows, ok := res["data"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 1)
	row, ok := rows[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"destroy"}, row["destructive_actions"])
	assert.Equal(t, []any{"apply"}, row["reversible_actions"])
}

// TestRegistryClassificationNeedsNoExecution states the load-bearing property
// behind all of the above: reading the declarations is a registry query. If
// this ever starts needing a live plugin, the CLI surface stops being usable
// on a machine where the plugin binary is absent.
func TestRegistryClassificationNeedsNoExecution(t *testing.T) {
	dir := writeModulePluginDir(t, "cloud-apply", []string{"destroy"}, []string{"apply"})
	registryPath := filepath.Join(t.TempDir(), "registry.db")
	registry, err := plugin.NewRegistry(context.Background(), registryPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = registry.Close() })

	bin := filepath.Join(dir, "missing-after-install")
	rec, err := registry.Install(context.Background(), plugin.PluginMeta{
		Name:               "cloud-apply",
		Version:            "1.0.0",
		Type:               plugin.TypeModule,
		EntryPoint:         "plugin",
		DestructiveActions: []string{"destroy"},
		ReversibleActions:  []string{"apply"},
	}, bin, "", false)
	require.NoError(t, err)
	assert.Equal(t, []string{"destroy"}, rec.DestructiveActions)
	assert.Equal(t, []string{"apply"}, rec.ReversibleActions)
}
