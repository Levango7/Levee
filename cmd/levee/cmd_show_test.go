package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShowCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("show")
	require.NotNil(t, cmd, "show subcommand should be registered")
	assert.Equal(t, "show", cmd.Name())
}

func TestShowCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("show")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "show without run-id arg should be rejected by Args validator")
}

func TestShowCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("show")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"a", "b"})
	assert.Error(t, err, "show with too many args should be rejected by Args validator")
}

func TestShowCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run": map[string]any{
			"id":     "run-001",
			"status": "running",
		},
		"batches": []map[string]any{
			{"batch_no": 1, "status": "completed"},
		},
		"steps":  []map[string]any{},
		"traces": []map[string]any{},
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
	runData, ok := result["run"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "run-001", runData["id"])
}
