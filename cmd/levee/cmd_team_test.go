package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/permission"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTeamCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("team")
	require.NotNil(t, cmd, "team subcommand should be registered")
	assert.Equal(t, "team", cmd.Name())
}

func TestTeamSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("team")
	require.NotNil(t, cmd)

	subNames := []string{"list", "add"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "team should have %q subcommand", name)
	}
}

func TestTeamListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("team")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "team list should not accept args")
}

func TestTeamAddCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("team")
	require.NotNil(t, cmd)

	addCmd := findSubCmd(cmd, "add")
	require.NotNil(t, addCmd)

	for _, flag := range []string{"name", "env"} {
		f := addCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "team add should have --%s flag", flag)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s flag should be required", flag)
	}
}

func TestTeamListCmdFormatFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("team")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	f := listCmd.Flags().Lookup("format")
	require.NotNil(t, f, "team list should have --format flag")
}

func TestPermissionConfigLoadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "permissions.yaml")

	cfg := &permission.PermissionConfig{
		Teams: []permission.TeamRule{
			{
				Name: "sre",
				Environments: []permission.EnvPermission{
					{Name: "dev", Actions: []string{"plan", "apply", "view"}},
					{Name: "prod", Actions: []string{"plan", "view"}},
				},
			},
		},
	}

	err := savePermissionConfig(path, cfg)
	require.NoError(t, err)

	loaded, err := loadPermissionConfig(path)
	require.NoError(t, err)
	require.Len(t, loaded.Teams, 1)
	assert.Equal(t, "sre", loaded.Teams[0].Name)
	require.Len(t, loaded.Teams[0].Environments, 2)
	assert.Equal(t, "dev", loaded.Teams[0].Environments[0].Name)
}

func TestPermissionConfigLoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	cfg, err := loadPermissionConfig(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Teams)
}

func TestPermissionConfigSaveCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "permissions.yaml")

	cfg := &permission.PermissionConfig{
		Teams: []permission.TeamRule{
			{Name: "sre", Environments: []permission.EnvPermission{
				{Name: "dev", Actions: []string{"plan"}},
			}},
		},
	}
	err := savePermissionConfig(path, cfg)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestDefaultTeamActions(t *testing.T) {
	actions := defaultTeamActions()
	assert.Contains(t, actions, permission.ActionPlan)
	assert.Contains(t, actions, permission.ActionApply)
	assert.Contains(t, actions, permission.ActionView)
}

func TestPrintTeamListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{
			"team": "sre",
			"environments": []map[string]any{
				{"env": "dev", "actions": []string{"plan", "apply", "view"}},
				{"env": "prod", "actions": []string{"plan", "view"}},
			},
		},
	}

	var buf bytes.Buffer
	printTeamListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "sre")
	assert.Contains(t, out, "dev")
	assert.Contains(t, out, "prod")
}

func TestPrintTeamListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printTeamListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No teams found")
}

func TestPrintTeamAddHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"team":    "sre",
		"env":     "prod",
		"actions": []string{"plan", "apply", "view"},
	}

	var buf bytes.Buffer
	printTeamAddHuman(&buf, output)
	assert.Contains(t, buf.String(), "sre")
	assert.Contains(t, buf.String(), "prod")
}

func TestTeamListOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"team": "sre", "environments": []map[string]any{}},
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  data,
		"meta":  nil,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
}
