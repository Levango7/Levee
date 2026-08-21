package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

func TestTraceCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("trace")
	require.NotNil(t, cmd, "trace subcommand should be registered")
	assert.Equal(t, "trace", cmd.Name())
}

func TestTraceCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("trace")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("verify")
	require.NotNil(t, f, "trace command should have --verify flag")
	assert.Equal(t, "false", f.DefValue, "verify flag should default to false")
}

func TestTraceCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("trace")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "trace without run-id arg should be rejected")
}

func TestTraceCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("trace")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "trace with too many args should be rejected")
}

func TestBuildTraceOutput(t *testing.T) {
	traces := []*state.Trace{
		{
			ID:        "trace-001",
			RunID:     "run-001",
			Event:     "step_execute",
			Actor:     "system",
			PrevHash:  "",
			CurrHash:  "abc123",
			Timestamp: state.Trace{}.Timestamp,
		},
	}

	output := buildTraceOutput("run-001", traces, nil)
	assert.Equal(t, "run-001", output["run_id"])
	assert.Equal(t, 1, output["count"])

	entries, ok := output["entries"].([]map[string]any)
	require.True(t, ok, "entries should be []map[string]any")
	assert.Len(t, entries, 1)
	assert.Equal(t, "step_execute", entries[0]["event"])
	assert.Equal(t, "abc123", entries[0]["curr_hash"])
}

func TestBuildTraceOutputWithVerify(t *testing.T) {
	traces := []*state.Trace{
		{ID: "trace-001", Event: "step_execute", Actor: "system", CurrHash: "abc123"},
	}

	vr := &audit.VerifyResult{
		RunID: "run-001",
		Valid: true,
		Count: 1,
	}

	output := buildTraceOutput("run-001", traces, vr)
	assert.Equal(t, "run-001", output["run_id"])

	verifyMap, ok := output["verify"].(map[string]any)
	require.True(t, ok, "verify should be present")
	assert.Equal(t, true, verifyMap["valid"])
	assert.Equal(t, 1, verifyMap["count"])
}

func TestShortHash(t *testing.T) {
	cases := []struct {
		input any
		want  string
	}{
		{"", "?"},
		{"abc", "abc"},
		{"0123456789abcdef0123456789abcdef", "01234567"},
		{42, "?"},
		{nil, "?"},
	}

	for _, tc := range cases {
		got := shortHash(tc.input)
		assert.Equal(t, tc.want, got, "shortHash(%v)", tc.input)
	}
}

func TestTraceCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":  "run-001",
		"count":   0,
		"entries": []map[string]any{},
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

func TestTraceCmdVerifyFailedExitCode(t *testing.T) {
	defer resetRootFlags()

	err := fmt.Errorf("chain verification failed for run %q: %d of %d records tampered [exit=6]",
		"run-001", 1, 5)
	assert.Equal(t, 6, exitCodeFor(err))
}

func TestPrintTraceHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id": "run-001",
		"count":  1,
		"entries": []map[string]any{
			{
				"event":     "step_execute",
				"actor":     "system",
				"timestamp": "2026-08-15T10:00:00Z",
				"prev_hash": "",
				"curr_hash": "0123456789abcdef0123456789abcdef",
			},
		},
	}

	var buf bytes.Buffer
	printTraceHuman(&buf, output)
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "step_execute")
}

func TestPrintTraceHumanWithVerify(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id":  "run-001",
		"count":   0,
		"entries": []map[string]any{},
		"verify": map[string]any{
			"valid":    true,
			"count":    5,
			"failures": []audit.ChainFailure{},
		},
	}

	var buf bytes.Buffer
	printTraceHuman(&buf, output)
	assert.Contains(t, buf.String(), "Chain verification")
	assert.Contains(t, buf.String(), "Valid")
}
