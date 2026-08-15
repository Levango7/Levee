package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogsCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("logs")
	require.NotNil(t, cmd, "logs subcommand should be registered")
	assert.Equal(t, "logs", cmd.Name())
}

func TestLogsCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("logs")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("target")
	require.NotNil(t, f, "logs command should have --target flag")

	ff := cmd.Flags().Lookup("follow")
	require.NotNil(t, ff, "logs command should have -f flag")
}

func TestLogsCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("logs")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "logs without run-id arg should be rejected")
}

func TestLogsCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("logs")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "logs with too many args should be rejected")
}

func TestBuildLogsOutput(t *testing.T) {
	now := time.Now().UTC()
	traces := []*state.Trace{
		{
			ID:        "trace-001",
			RunID:     "run-001",
			Event:     "step_execute",
			Actor:     "system",
			Detail:    `{"target":"host-1","action":"pkg.install"}`,
			Timestamp: now,
		},
		{
			ID:        "trace-002",
			RunID:     "run-001",
			Event:     "gate_check",
			Actor:     "system",
			Detail:    `{"target":"*"}`,
			Timestamp: now,
		},
	}

	output := buildLogsOutput("run-001", traces)
	assert.Equal(t, "run-001", output["run_id"])
	assert.Equal(t, 2, output["count"])

	entries, ok := output["entries"].([]map[string]any)
	require.True(t, ok, "entries should be []map[string]any")
	assert.Len(t, entries, 2)
	assert.Equal(t, "step_execute", entries[0]["event"])
	assert.Equal(t, "host-1", entries[0]["target"])
}

func TestFilterTracesByTarget(t *testing.T) {
	traces := []*state.Trace{
		{ID: "t1", Detail: `{"target":"host-1"}`},
		{ID: "t2", Detail: `{"target":"host-2"}`},
		{ID: "t3", Detail: `{"target":"host-1"}`},
		{ID: "t4", Detail: `{"other":"value"}`},
	}

	filtered := filterTracesByTarget(traces, "host-1")
	assert.Len(t, filtered, 2)
	assert.Equal(t, "t1", filtered[0].ID)
	assert.Equal(t, "t3", filtered[1].ID)
}

func TestFilterTracesByTargetNoMatch(t *testing.T) {
	traces := []*state.Trace{
		{ID: "t1", Detail: `{"target":"host-1"}`},
	}

	filtered := filterTracesByTarget(traces, "host-99")
	assert.Len(t, filtered, 0)
}

func TestIsTerminalStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"completed", true},
		{"failed", true},
		{"cancelled", true},
		{"rolled_back", true},
		{"running", false},
		{"pending", false},
		{"paused", false},
		{"rolling_back", false},
	}

	for _, tc := range cases {
		got := isTerminalStatus(tc.status)
		assert.Equal(t, tc.want, got, "isTerminalStatus(%q)", tc.status)
	}
}

func TestLogsCmdOutputEnvelope(t *testing.T) {
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

func TestPrintLogsHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id": "run-001",
		"count":  1,
		"entries": []map[string]any{
			{"event": "step_execute", "actor": "system", "timestamp": "2026-08-15T10:00:00Z"},
		},
	}

	var buf bytes.Buffer
	printLogsHuman(&buf, output)
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "step_execute")
}

func TestPrintTraceRecord(t *testing.T) {
	now := time.Now().UTC()
	trace := &state.Trace{
		ID:        "trace-001",
		Event:     "step_execute",
		Actor:     "system",
		Timestamp: now,
	}

	var buf bytes.Buffer
	printTraceRecord(&buf, trace)
	assert.Contains(t, buf.String(), "step_execute")
	assert.Contains(t, buf.String(), "system")
}
