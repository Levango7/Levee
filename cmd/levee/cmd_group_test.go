package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroupCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("group")
	require.NotNil(t, cmd, "group subcommand should be registered")
	assert.Equal(t, "group", cmd.Name())
}

func TestGroupAddCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("group")
	require.NotNil(t, cmd)
	add, _, err := cmd.Find([]string{"add"})
	require.NoError(t, err)
	require.NotNil(t, add, "group add subcommand should be registered")
	f := add.Flags().Lookup("parent")
	require.NotNil(t, f, "group add should have --parent flag")
}

// --- newGroupID (SA-012) -----------------------------------------------------

func TestNewGroupID(t *testing.T) {
	id, err := newGroupID()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(id, "grp-"), "id %q should carry the grp- prefix", id)
	assert.Len(t, id, len("grp-")+10, "id should embed 5 random bytes as hex")
}

func TestNewGroupIDUniqueness(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		id, err := newGroupID()
		require.NoError(t, err)
		require.False(t, seen[id], "duplicate group id generated: %s", id)
		seen[id] = true
	}
}

// TestGroupListEmptyJSON_ArrayNotNull guards the CLI JSON contract:
// an empty group list must serialize as `"data": []`, never `"data": null`.
func TestGroupListEmptyJSON_ArrayNotNull(t *testing.T) {
	defer resetRootFlags()
	t.Cleanup(resetRootFlags)

	env := newCLIEnv(t)
	out, err := executeCommand("--config", env.cfgPath, "group", "list", "--json")
	require.NoError(t, err, "output: %s", out)

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &envelope), "output: %s", out)
	assert.Equal(t, "[]", strings.TrimSpace(string(envelope.Data)), "empty group list must be [], not null; output: %s", out)
}
