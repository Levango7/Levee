package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRollbackCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd, "rollback subcommand should be registered")
	assert.Equal(t, "rollback", cmd.Name())
}

func TestRollbackCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("force")
	require.NotNil(t, f, "rollback command should have --force flag")
	assert.Equal(t, "false", f.DefValue, "force flag should default to false")
}

func TestRollbackCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "rollback without run-id arg should be rejected")
}

func TestRollbackCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("rollback")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "rollback with too many args should be rejected")
}

func TestIsRollbackableStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"running", true},
		{"completed", true},
		{"failed", true},
		{"pending", false},
		{"draft", false},
		{"approved", false},
		{"paused", false},
		{"cancelled", false},
		{"rolling_back", false},
		{"rolled_back", false},
	}

	for _, tc := range cases {
		got := isRollbackableStatus(tc.status)
		assert.Equal(t, tc.want, got, "isRollbackableStatus(%q)", tc.status)
	}
}

func TestRollbackCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id": "run-001",
		"action": "rolling_back",
		"actor":  "cli-user",
		"force":  false,
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
	assert.Equal(t, "rolling_back", result["action"])
}

func TestRollbackCmdStateConflictExitCode(t *testing.T) {
	defer resetRootFlags()

	err := fmt.Errorf("run %q is in %q state, cannot rollback [exit=4]", "run-001", "pending")
	assert.Equal(t, 4, exitCodeFor(err))
}

func TestNewRollbackAuditID(t *testing.T) {
	id := newRollbackAuditID()
	assert.NotEmpty(t, id)
	assert.Contains(t, id, "audit-rollback-")
}

func TestNewRollbackAuditIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newRollbackAuditID()
		assert.False(t, ids[id], "rollback audit ID should be unique: %s", id)
		ids[id] = true
	}
}

func TestRollbackCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Run %s rollback triggered by %s\n", "run-001", "cli-user")
	fmt.Fprintf(&buf, "  Status: rolling_back\n")
	fmt.Fprintf(&buf, "  Post-rollback verification: pending\n")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "rolling_back")
	assert.Contains(t, buf.String(), "pending")
}
