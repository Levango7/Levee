package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("list")
	require.NotNil(t, cmd, "list subcommand should be registered")
	assert.Equal(t, "list", cmd.Name())
}

func TestListCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("list")
	require.NotNil(t, cmd)

	cases := []struct {
		name     string
		defValue string
	}{
		{"status", ""},
		{"template", ""},
		{"limit", "20"},
		{"offset", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := cmd.Flags().Lookup(tc.name)
			require.NotNil(t, f, "list command should have --%s flag", tc.name)
			assert.Equal(t, tc.defValue, f.DefValue)
		})
	}
}

func TestListCmdDisallowsArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("list")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"unexpected"})
	assert.Error(t, err, "list with positional args should be rejected by Args validator")
}

func TestListCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	rows := []map[string]any{
		{"id": "run-001", "status": "running"},
		{"id": "run-002", "status": "completed"},
	}
	meta := map[string]any{
		"limit":  20,
		"offset": 0,
		"count":  2,
	}

	var buf bytes.Buffer
	require.NoError(t, PrintJSON(&buf, map[string]any{
		"data":  rows,
		"meta":  meta,
		"error": nil,
	}))

	var env outputEnvelope
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	assert.NotNil(t, env.Data)
	assert.NotNil(t, env.Meta)

	// Verify data is a slice.
	raw, err := json.Marshal(env.Data)
	require.NoError(t, err)
	var result []any
	require.NoError(t, json.Unmarshal(raw, &result))
	assert.Len(t, result, 2)
}
