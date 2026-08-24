package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"
)

func TestTargetCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd, "target subcommand should be registered")
	assert.Equal(t, "target", cmd.Name())
}

func TestTargetSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd)

	subNames := []string{"list", "import", "check"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "target should have %q subcommand", name)
	}
}

func TestTargetListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "target list should not accept args")
}

func TestTargetCheckCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd)

	checkCmd := findSubCmd(cmd, "check")
	require.NotNil(t, checkCmd)

	err := checkCmd.Args(checkCmd, []string{})
	assert.Error(t, err, "target check requires host arg")
}

func TestTargetImportCmdFileFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd)

	importCmd := findSubCmd(cmd, "import")
	require.NotNil(t, importCmd)

	f := importCmd.Flags().Lookup("file")
	require.NotNil(t, f, "target import should have --file flag")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--file flag should be required")
}

func TestTargetListCmdFormatFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("target")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	f := listCmd.Flags().Lookup("format")
	require.NotNil(t, f, "target list should have --format flag")
}

func TestTargetOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"host": "web-01.example.com"},
		{"host": "db-01.example.com"},
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

func TestPrintTargetListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []*state.Target{
		{Hostname: "web-01.example.com", Port: 22, ChannelType: "ssh", Status: state.StatusActive,
			Labels: map[string]string{"env": "prod"}},
		{Hostname: "db-01.example.com", Port: 22, ChannelType: "ssh", Status: state.StatusActive},
	}

	var buf bytes.Buffer
	printTargetListHuman(&buf, rows)
	assert.Contains(t, buf.String(), "web-01.example.com")
	assert.Contains(t, buf.String(), "db-01.example.com")
	assert.Contains(t, buf.String(), "env=prod")
}

func TestPrintTargetListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printTargetListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No targets in the inventory")
}

func TestPrintTargetImportHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"file":    "inventory.yaml",
		"created": 5,
		"updated": 2,
		"failed":  0,
	}

	var buf bytes.Buffer
	printTargetImportHuman(&buf, output)
	assert.Contains(t, buf.String(), "5")
	assert.Contains(t, buf.String(), "inventory.yaml")
}

func TestPrintTargetCheckHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"target":    "web-01.example.com",
		"reachable": true,
		"latency":   "5ms",
		"error":     "",
	}

	var buf bytes.Buffer
	printTargetCheckHuman(&buf, output)
	assert.Contains(t, buf.String(), "web-01.example.com")
	assert.Contains(t, buf.String(), "Reachable")
}

func TestSimpleTarget(t *testing.T) {
	defer resetRootFlags()

	tgt := &simpleTarget{host: "myhost"}
	assert.Equal(t, "myhost", tgt.Host())
	assert.Equal(t, 0, tgt.Port())
	assert.Equal(t, "ssh", tgt.Type())
	assert.Equal(t, channel.CredentialRef{}, tgt.Credentials())
}
