package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloneCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("clone")
	require.NotNil(t, cmd, "clone subcommand should be registered")
	assert.Equal(t, "clone", cmd.Name())
}

func TestCloneCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("clone")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "clone without run-id arg should be rejected by Args validator")
}

func TestCloneCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("clone")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"a", "b"})
	assert.Error(t, err, "clone with too many args should be rejected by Args validator")
}

func TestCloneCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"original_run_id": "run-001",
		"cloned_run_id":   "run-002",
		"cloned_by":       "cli-user",
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
	assert.Equal(t, "run-001", result["original_run_id"])
	assert.Equal(t, "run-002", result["cloned_run_id"])
}
