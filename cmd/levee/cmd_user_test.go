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

func TestUserCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("user")
	require.NotNil(t, cmd, "user subcommand should be registered")
	assert.Equal(t, "user", cmd.Name())
}

func TestUserSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("user")
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
		assert.True(t, found, "user should have %q subcommand", name)
	}
}

func TestUserListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("user")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "user list should not accept args")
}

func TestUserAddCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("user")
	require.NotNil(t, cmd)

	addCmd := findSubCmd(cmd, "add")
	require.NotNil(t, addCmd)

	for _, flag := range []string{"name", "team", "role"} {
		f := addCmd.Flags().Lookup(flag)
		require.NotNil(t, f, "user add should have --%s flag", flag)
		_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
		assert.True(t, required, "--%s flag should be required", flag)
	}
}

func TestUserListCmdFormatFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("user")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	f := listCmd.Flags().Lookup("format")
	require.NotNil(t, f, "user list should have --format flag")
}

func TestUserRegistryLoadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.yaml")

	reg := &userRegistry{
		Users: []userEntry{
			{Name: "alice", Team: "sre", Role: "admin"},
			{Name: "bob", Team: "dba", Role: "operator"},
		},
	}

	err := saveUserRegistry(path, reg)
	require.NoError(t, err)

	loaded, err := loadUserRegistry(path)
	require.NoError(t, err)
	assert.Len(t, loaded.Users, 2)
	assert.Equal(t, "alice", loaded.Users[0].Name)
	assert.Equal(t, "sre", loaded.Users[0].Team)
	assert.Equal(t, "admin", loaded.Users[0].Role)
	assert.Equal(t, "bob", loaded.Users[1].Name)
}

func TestUserRegistryLoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.yaml")

	reg, err := loadUserRegistry(path)
	require.NoError(t, err)
	assert.Empty(t, reg.Users)
}

func TestUserRegistrySaveCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "users.yaml")

	reg := &userRegistry{Users: []userEntry{{Name: "alice", Team: "sre", Role: "admin"}}}
	err := saveUserRegistry(path, reg)
	require.NoError(t, err)

	_, err = os.Stat(path)
	require.NoError(t, err)
}

func TestPrintUserListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"name": "alice", "team": "sre", "role": "admin"},
		{"name": "bob", "team": "dba", "role": "operator"},
	}

	var buf bytes.Buffer
	printUserListHuman(&buf, rows)
	out := buf.String()
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "sre")
	assert.Contains(t, out, "admin")
	assert.Contains(t, out, "bob")
}

func TestPrintUserListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printUserListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No users found")
}

func TestPrintUserAddHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name": "alice",
		"team": "sre",
		"role": "admin",
	}

	var buf bytes.Buffer
	printUserAddHuman(&buf, output)
	assert.Contains(t, buf.String(), "alice")
	assert.Contains(t, buf.String(), "sre")
	assert.Contains(t, buf.String(), "admin")
}

func TestUserListOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"name": "alice", "team": "sre", "role": "admin"},
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

func TestUsersFilePath(t *testing.T) {
	path := usersFilePath("/home/user/.levee/data")
	expected := filepath.Join("/home/user/.levee/data", "users.yaml")
	assert.Equal(t, expected, path)
}
