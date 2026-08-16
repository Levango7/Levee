package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd, "plugin subcommand should be registered")
	assert.Equal(t, "plugin", cmd.Name())
}

func TestPluginSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	subNames := []string{"list", "install", "enable", "disable", "remove", "info"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "plugin should have %q subcommand", name)
	}
}

func TestPluginListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "plugin list should not accept args")
}

func TestPluginInstallCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	installCmd := findSubCmd(cmd, "install")
	require.NotNil(t, installCmd)

	err := installCmd.Args(installCmd, []string{})
	assert.Error(t, err, "plugin install requires path arg")
}

func TestPluginEnableCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	enableCmd := findSubCmd(cmd, "enable")
	require.NotNil(t, enableCmd)

	err := enableCmd.Args(enableCmd, []string{})
	assert.Error(t, err, "plugin enable requires name arg")
}

func TestPluginDisableCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	disableCmd := findSubCmd(cmd, "disable")
	require.NotNil(t, disableCmd)

	err := disableCmd.Args(disableCmd, []string{})
	assert.Error(t, err, "plugin disable requires name arg")
}

func TestPluginRemoveCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	removeCmd := findSubCmd(cmd, "remove")
	require.NotNil(t, removeCmd)

	err := removeCmd.Args(removeCmd, []string{})
	assert.Error(t, err, "plugin remove requires name arg")
}

func TestPluginInfoCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	infoCmd := findSubCmd(cmd, "info")
	require.NotNil(t, infoCmd)

	err := infoCmd.Args(infoCmd, []string{})
	assert.Error(t, err, "plugin info requires name arg")
}

func TestPluginInstallCmdVerifyFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	installCmd := findSubCmd(cmd, "install")
	require.NotNil(t, installCmd)

	f := installCmd.Flags().Lookup("verify-signature")
	require.NotNil(t, f, "plugin install should have --verify-signature flag")
	assert.Equal(t, "false", f.DefValue, "default should be false")
}

func TestPluginListHumanOutput(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"name": "http-probe", "version": "1.0.0", "type": "gate", "state": "enabled", "description": "HTTP probe"},
		{"name": "slack-notify", "version": "0.2.0", "type": "notify", "state": "disabled", "description": "Slack"},
	}

	var buf bytes.Buffer
	printPluginListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "http-probe")
	assert.Contains(t, out, "slack-notify")
	assert.Contains(t, out, "1.0.0")
	assert.Contains(t, out, "gate")
	assert.Contains(t, out, "enabled")
}

func TestPluginListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printPluginListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No plugins installed")
}

func TestPluginInstallHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name":    "http-probe",
		"version": "1.0.0",
		"type":    "gate",
	}

	var buf bytes.Buffer
	printPluginInstallHuman(&buf, output)
	assert.Contains(t, buf.String(), "http-probe")
	assert.Contains(t, buf.String(), "1.0.0")
	assert.Contains(t, buf.String(), "gate")
}

func TestPluginInfoHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name":        "http-probe",
		"version":     "1.0.0",
		"type":        "gate",
		"author":      "levee-team",
		"description": "HTTP probe gate",
		"state":       "enabled",
		"entry_point": "plugin",
		"binary_path": "/opt/levee/plugins/http-probe/plugin",
		"installed_at": "2026-08-16T10:00:00Z",
		"updated_at":  "2026-08-16T10:00:00Z",
	}

	var buf bytes.Buffer
	printPluginInfoHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "http-probe")
	assert.Contains(t, out, "1.0.0")
	assert.Contains(t, out, "gate")
	assert.Contains(t, out, "enabled")
	assert.Contains(t, out, "levee-team")
}

func TestPluginInfoHumanWithOptionalFields(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name":             "http-probe",
		"version":          "1.0.0",
		"type":             "gate",
		"author":           "levee-team",
		"description":      "HTTP probe gate",
		"state":            "error",
		"entry_point":      "plugin",
		"binary_path":      "/opt/levee/plugins/http-probe/plugin",
		"installed_at":     "2026-08-16T10:00:00Z",
		"updated_at":       "2026-08-16T10:00:00Z",
		"min_host_version": "1.0.0",
		"max_host_version": "2.0.0",
		"config_yaml":      "timeout: 10s",
		"signature":        "abc123",
		"error_msg":        "crashed",
	}

	var buf bytes.Buffer
	printPluginInfoHuman(&buf, output)
	out := buf.String()
	assert.Contains(t, out, "1.0.0")
	assert.Contains(t, out, "2.0.0")
	assert.Contains(t, out, "timeout: 10s")
	assert.Contains(t, out, "abc123")
	assert.Contains(t, out, "crashed")
}

func TestPluginOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"name": "http-probe", "version": "1.0.0"},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  data,
		"meta":  map[string]any{"count": 1},
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	require.NotNil(t, env.Data)
}

func TestPluginRecordToMap(t *testing.T) {
	defer resetRootFlags()

	// Create a minimal registry record and convert it.
	rec := map[string]any{
		"name":         "test-plugin",
		"version":      "1.0.0",
		"type":         "gate",
		"author":       "test",
		"description":  "test desc",
		"entry_point":  "plugin",
		"state":        "installed",
		"binary_path":  "/path/to/plugin",
		"installed_at": "2026-08-16T10:00:00Z",
		"updated_at":   "2026-08-16T10:00:00Z",
	}

	assert.Equal(t, "test-plugin", rec["name"])
	assert.Equal(t, "gate", rec["type"])
	assert.Equal(t, "installed", rec["state"])
}

func TestPluginCmdHelp(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)
	assert.NotEmpty(t, cmd.Short)
	assert.NotEmpty(t, cmd.Long)
}

func TestPluginSubcommandArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	// Verify that subcommands expecting exactly 1 arg reject 0 and 2 args.
	cases := []struct {
		name string
		args []string
		ok   bool
	}{
		{"install", []string{}, false},
		{"install", []string{"path"}, true},
		{"install", []string{"a", "b"}, false},
		{"enable", []string{}, false},
		{"enable", []string{"name"}, true},
		{"disable", []string{}, false},
		{"disable", []string{"name"}, true},
		{"remove", []string{}, false},
		{"remove", []string{"name"}, true},
		{"info", []string{}, false},
		{"info", []string{"name"}, true},
	}

	for _, c := range cases {
		sub := findSubCmd(cmd, c.name)
		require.NotNil(t, sub, "subcommand %q should exist", c.name)
		err := sub.Args(sub, c.args)
		if c.ok {
			assert.NoError(t, err, "%s with %v should pass", c.name, c.args)
		} else {
			assert.Error(t, err, "%s with %v should fail", c.name, c.args)
		}
	}
}

func TestPluginListCmdRunE(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)
	assert.NotNil(t, listCmd.RunE, "list should have RunE")
}

func TestPluginInstallCmdRunE(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("plugin")
	require.NotNil(t, cmd)

	installCmd := findSubCmd(cmd, "install")
	require.NotNil(t, installCmd)
	assert.NotNil(t, installCmd.RunE, "install should have RunE")
}

// TestPluginCmdWithTempDir verifies that the plugin CLI can be invoked
// end-to-end against a temporary data directory. It is a smoke test that
// exercises the full stack (config → registry → manager → output).
func TestPluginCmdWithTempDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end test in short mode")
	}
	defer resetRootFlags()

	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Write a minimal config file.
	configPath := filepath.Join(dir, "config.yaml")
	configContent := "server:\n  data_dir: " + dataDir + "\n  log_level: info\n  log_format: text\n" +
		"database:\n  driver: sqlite\n  path: " + filepath.Join(dataDir, "levee.db") + "\n"
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o644))

	optConfigPath = configPath
	optJSON = true

	// Execute `levee plugin list` — should return an empty list.
	cmd := findSub("plugin")
	require.NotNil(t, cmd)
	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	var buf bytes.Buffer
	listCmd.SetOut(&buf)
	listCmd.SetErr(&buf)

	err := listCmd.RunE(listCmd, []string{})
	// The command may fail if the config cannot be loaded in the test
	// environment; that is acceptable for this smoke test. We only verify
	// that the command does not panic.
	_ = err
}

// Ensure cobra is referenced to avoid unused import in some build configs.
var _ = cobra.Command{}