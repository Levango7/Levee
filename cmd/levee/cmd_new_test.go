package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("new")
	require.NotNil(t, cmd, "new subcommand should be registered")
	assert.Equal(t, "new", cmd.Name())
	assert.Contains(t, cmd.Use, "<template>")
}

func TestNewCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("new")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("params")
	require.NotNil(t, f, "new command should have --params flag")
	assert.Equal(t, "", f.DefValue, "params default should be empty")
}

func TestNewCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("new")
	require.NotNil(t, cmd)

	// Cobra's ExactArgs(1) should reject empty args.
	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "new without template arg should be rejected by Args validator")
}

func TestNewCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("new")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"a", "b"})
	assert.Error(t, err, "new with too many args should be rejected by Args validator")
}

func TestNewCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	// Verify that the output structure matches the envelope format.
	data := map[string]any{
		"run_id":        "run-abc123",
		"template_name": "db-migrate",
		"content":       "workflow: ...",
		"params":        map[string]string{"table": "orders"},
		"status":        "draft",
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

	// Re-marshal and check key fields.
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Equal(t, "run-abc123", result["run_id"])
	assert.Equal(t, "draft", result["status"])
}
