package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTemplateCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd, "template subcommand should be registered")
	assert.Equal(t, "template", cmd.Name())
}

func TestTemplateSubcommands(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	subNames := []string{"list", "show", "create", "delete"}
	for _, name := range subNames {
		found := false
		for _, sub := range cmd.Commands() {
			if sub.Name() == name {
				found = true
				break
			}
		}
		assert.True(t, found, "template should have %q subcommand", name)
	}
}

func TestTemplateListCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	err := listCmd.Args(listCmd, []string{"unexpected"})
	assert.Error(t, err, "template list should not accept args")
}

func TestTemplateShowCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	showCmd := findSubCmd(cmd, "show")
	require.NotNil(t, showCmd)

	err := showCmd.Args(showCmd, []string{})
	assert.Error(t, err, "template show requires name arg")
}

func TestTemplateDeleteCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	deleteCmd := findSubCmd(cmd, "delete")
	require.NotNil(t, deleteCmd)

	err := deleteCmd.Args(deleteCmd, []string{})
	assert.Error(t, err, "template delete requires name arg")
}

func TestTemplateCreateCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	createCmd := findSubCmd(cmd, "create")
	require.NotNil(t, createCmd)

	for _, name := range []string{"name", "description", "content", "params"} {
		f := createCmd.Flags().Lookup(name)
		require.NotNil(t, f, "template create should have --%s flag", name)
	}

	// name and content are required.
	nameFlag := createCmd.Flags().Lookup("name")
	require.NotNil(t, nameFlag)
	_, nameRequired := nameFlag.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, nameRequired, "--name flag should be required")

	contentFlag := createCmd.Flags().Lookup("content")
	require.NotNil(t, contentFlag)
	_, contentRequired := contentFlag.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, contentRequired, "--content flag should be required")
}

func TestTemplateListCmdTagFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("template")
	require.NotNil(t, cmd)

	listCmd := findSubCmd(cmd, "list")
	require.NotNil(t, listCmd)

	f := listCmd.Flags().Lookup("tag")
	require.NotNil(t, f, "template list should have --tag flag")
}

func TestTemplateOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := []map[string]any{
		{"id": "tmpl-001", "name": "restart-nginx", "description": "Restart nginx"},
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

func TestPrintTemplateListHuman(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"id": "tmpl-001", "name": "restart-nginx", "description": "Restart nginx service"},
		{"id": "tmpl-002", "name": "patch-os", "description": "OS patching"},
	}

	var buf bytes.Buffer
	printTemplateListHuman(&buf, rows)
	assert.Contains(t, buf.String(), "restart-nginx")
	assert.Contains(t, buf.String(), "patch-os")
}

func TestPrintTemplateListHumanEmpty(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	printTemplateListHuman(&buf, nil)
	assert.Contains(t, buf.String(), "No templates found")
}

func TestPrintTemplateCreateHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"id":   "tmpl-abc",
		"name": "my-template",
	}

	var buf bytes.Buffer
	printTemplateCreateHuman(&buf, output)
	assert.Contains(t, buf.String(), "my-template")
	assert.Contains(t, buf.String(), "tmpl-abc")
}

func TestPrintTemplateDeleteHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"name":    "old-template",
		"deleted": true,
	}

	var buf bytes.Buffer
	printTemplateDeleteHuman(&buf, output)
	assert.Contains(t, buf.String(), "old-template")
}

// findSubCmd returns the sub-command of parent with the given name.
func findSubCmd(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}
