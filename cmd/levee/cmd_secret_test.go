package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd, "secret subcommand should be registered")
	assert.Equal(t, "secret", cmd.Name())
}

func TestSecretSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	subNames := []string{"list", "add", "rotate", "revoke", "show"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "secret should have %q subcommand", name)
	}
}

func TestSecretListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "secret list should not accept args")
}

func TestSecretAddCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	addCmd := findSubCmd(cmd, "add")
	require.NotNil(t, addCmd)

	for _, name := range []string{"name", "type", "value"} {
		f := addCmd.Flags().Lookup(name)
		require.NotNil(t, f, "secret add should have --%s flag", name)
	}

	// name and value are required.
	nameFlag := addCmd.Flags().Lookup("name")
	require.NotNil(t, nameFlag)
	_, nameRequired := nameFlag.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, nameRequired, "--name flag should be required")

	valueFlag := addCmd.Flags().Lookup("value")
	require.NotNil(t, valueFlag)
	_, valueRequired := valueFlag.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, valueRequired, "--value flag should be required")

	// type has default.
	typeFlag := addCmd.Flags().Lookup("type")
	require.NotNil(t, typeFlag)
	assert.Equal(t, "ssh_password", typeFlag.DefValue, "--type flag should default to ssh_password")
}

func TestSecretRotateCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	rotateCmd := findSubCmd(cmd, "rotate")
	require.NotNil(t, rotateCmd)

	f := rotateCmd.Flags().Lookup("name")
	require.NotNil(t, f, "secret rotate should have --name flag")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--name flag should be required")
}

func TestSecretRevokeCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	revokeCmd := findSubCmd(cmd, "revoke")
	require.NotNil(t, revokeCmd)

	f := revokeCmd.Flags().Lookup("name")
	require.NotNil(t, f, "secret revoke should have --name flag")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--name flag should be required")
}

func TestSecretShowCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("secret")
	require.NotNil(t, cmd)

	showCmd := findSubCmd(cmd, "show")
	require.NotNil(t, showCmd)

	f := showCmd.Flags().Lookup("name")
	require.NotNil(t, f, "secret show should have --name flag")
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "--name flag should be required")
}

func TestSecretOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"id": "cred-001", "name": "ssh-prod", "type": "ssh_key"},
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

func TestPrintSecretListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"id": "cred-001", "name": "ssh-prod", "type": "ssh_key", "created_at": "2026-08-15"},
	}

	var buf bytes.Buffer
	printSecretListHuman(&buf, rows)
	assert.Contains(t, buf.String(), "ssh-prod")
	assert.Contains(t, buf.String(), "ssh_key")
}

func TestPrintSecretListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printSecretListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No credentials found")
}

func TestPrintSecretAddHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"id":   "cred-abc",
		"name": "my-key",
		"type": "ssh_key",
	}

	var buf bytes.Buffer
	printSecretAddHuman(&buf, output)
	assert.Contains(t, buf.String(), "my-key")
	assert.Contains(t, buf.String(), "cred-abc")
}

func TestPrintSecretRotateHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"id":   "cred-abc",
		"name": "my-key",
	}

	var buf bytes.Buffer
	printSecretRotateHuman(&buf, output)
	assert.Contains(t, buf.String(), "my-key")
}

func TestPrintSecretRevokeHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name":    "old-key",
		"revoked": true,
	}

	var buf bytes.Buffer
	printSecretRevokeHuman(&buf, output)
	assert.Contains(t, buf.String(), "old-key")
}

func TestPrintSecretShowHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"id":         "cred-abc",
		"name":       "my-key",
		"type":       "ssh_key",
		"created_at": "2026-08-15",
		"rotated_at": nil,
	}

	var buf bytes.Buffer
	printSecretShowHuman(&buf, output)
	assert.Contains(t, buf.String(), "my-key")
	assert.Contains(t, buf.String(), "ssh_key")
}

func TestPrintSecretShowHumanWithRotation(t *testing.T) {
	defer resetRootFlags()

	now := "2026-08-15T12:00:00Z"
	output := map[string]any{
		"id":         "cred-abc",
		"name":       "my-key",
		"type":       "ssh_key",
		"created_at": "2026-08-15",
		"rotated_at": &now,
	}

	var buf bytes.Buffer
	printSecretShowHuman(&buf, output)
	assert.Contains(t, buf.String(), "my-key")
}
