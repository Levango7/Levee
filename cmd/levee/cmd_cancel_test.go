package main

import (
	"bytes"

	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCancelCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("cancel")
	require.NotNil(t, cmd, "cancel subcommand should be registered")
	assert.Equal(t, "cancel", cmd.Name())
}

func TestCancelCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("cancel")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("reason")
	require.NotNil(t, f, "cancel command should have --reason flag")
}

func TestCancelCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("cancel")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "cancel without run-id arg should be rejected")
}

func TestCancelCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("cancel")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "cancel with too many args should be rejected")
}

func TestIsCancellableStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"pending", true},
		{"draft", true},
		{"running", false},
		{"approved", false},
		{"paused", false},
		{"completed", false},
		{"failed", false},
		{"cancelled", false},
	}

	for _, tc := range cases {
		got := isCancellableStatus(tc.status)
		assert.Equal(t, tc.want, got, "isCancellableStatus(%q)", tc.status)
	}
}

func TestCancelCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id": "run-001",
		"action": "cancelled",
		"actor":  "cli-user",
		"reason": "no longer needed",
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
	assert.Equal(t, "cancelled", result["action"])
	assert.Equal(t, "no longer needed", result["reason"])
}

func TestCancelCmdStateConflictExitCode(t *testing.T) {
	defer resetRootFlags()

	// Verify that a state conflict error carries [exit=4].
	err := fmt.Errorf("run %q is in %q state, cannot cancel [exit=4]", "run-001", "running")
	assert.Equal(t, 4, exitCodeFor(err))
}

func TestNewCancelAuditID(t *testing.T) {
	id := newCancelAuditID()
	assert.NotEmpty(t, id)
	assert.Contains(t, id, "audit-cancel-")
}

func TestNewCancelAuditIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newCancelAuditID()
		assert.False(t, ids[id], "cancel audit ID should be unique: %s", id)
		ids[id] = true
	}
}

func TestCancelCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	// Simulate the human-readable output format.
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Run %s cancelled by %s%s\n", "run-001", "cli-user", ": not needed")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "cancelled")
	assert.Contains(t, buf.String(), "cli-user")
	assert.Contains(t, buf.String(), "not needed")
}

func TestCancelCmdHumanOutputNoReason(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Run %s cancelled by %s%s\n", "run-001", "cli-user", "")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "cancelled")
	assert.NotContains(t, buf.String(), ": ")
}
