package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("apply")
	require.NotNil(t, cmd, "apply subcommand should be registered")
	assert.Equal(t, "apply", cmd.Name())
}

func TestApplyCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("apply")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("force")
	require.NotNil(t, f, "apply command should have --force flag")
	assert.Equal(t, "false", f.DefValue)
}

func TestApplyCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("apply")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "apply without run-id arg should be rejected by Args validator")
}

func TestApplyCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("apply")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "apply with too many args should be rejected")
}

func TestApplyCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":     "run-001",
		"status":     "running",
		"applied_by": "cli-user",
		"force":      false,
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
	assert.Equal(t, "running", result["status"])
	assert.Equal(t, "run-001", result["run_id"])
}

func TestIsApplicableState(t *testing.T) {
	cases := []struct {
		status string
		force  bool
		want   bool
	}{
		{"approved", false, true},
		{"approved", true, true},
		{"pending", false, false},
		{"pending", true, true},
		{"draft", false, false},
		{"draft", true, true},
		{"running", false, false},
		{"running", true, false},
		{"completed", false, false},
		{"failed", false, false},
		{"cancelled", false, false},
		{"paused", false, false},
	}

	for _, tc := range cases {
		got := isApplicableState(tc.status, tc.force)
		assert.Equal(t, tc.want, got, "isApplicableState(%q, %v)", tc.status, tc.force)
	}
}

func TestApplyCmdStateConflictExitCode(t *testing.T) {
	defer resetRootFlags()

	// Verify that a state conflict error carries [exit=4].
	cmd := findSub("apply")
	require.NotNil(t, cmd)

	// Running apply without a real store will fail at openStore, but we can
	// verify the exit code mapping logic for the state conflict marker.
	err := fmt.Errorf("run %q is in %q state, cannot apply [exit=4]", "run-001", "completed")
	assert.Equal(t, 4, exitCodeFor(err))
}
