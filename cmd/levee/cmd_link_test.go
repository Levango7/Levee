package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("link")
	require.NotNil(t, cmd, "link subcommand should be registered")
	assert.Equal(t, "link", cmd.Name())
}

func TestLinkCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("link")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "link without run-id arg should be rejected")
}

func TestLinkCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("link")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"a", "b"})
	assert.Error(t, err, "link with too many args should be rejected")
}

func TestLinkCmdIncidentFlag(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("link")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("incident")
	require.NotNil(t, f, "link command should have --incident flag")
	assert.Equal(t, "", f.DefValue, "incident flag should default to empty")
}

func TestLinkCmdIncidentFlagRequired(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("link")
	require.NotNil(t, cmd)

	// Verify the flag is marked required by checking cobra's required annotations.
	f := cmd.Flags().Lookup("incident")
	require.NotNil(t, f)
	_, required := f.Annotations[cobra.BashCompOneRequiredFlag]
	assert.True(t, required, "incident flag should be marked required")
}

func TestLinkCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":      "run-001",
		"incident_id": "inc-001",
		"linked":      true,
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

	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, "run-001", result["run_id"])
	assert.Equal(t, "inc-001", result["incident_id"])
	assert.Equal(t, true, result["linked"])
}

func TestPrintLinkHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id":      "run-abc",
		"incident_id": "inc-xyz",
		"linked":      true,
	}

	var buf bytes.Buffer
	printLinkHuman(&buf, output)
	assert.Contains(t, buf.String(), "run-abc")
	assert.Contains(t, buf.String(), "inc-xyz")
	assert.Contains(t, buf.String(), "linked")
}
