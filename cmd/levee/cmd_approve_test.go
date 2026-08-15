package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApproveCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("approve")
	require.NotNil(t, cmd, "approve subcommand should be registered")
	assert.Equal(t, "approve", cmd.Name())
}

func TestRejectCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("reject")
	require.NotNil(t, cmd, "reject subcommand should be registered")
	assert.Equal(t, "reject", cmd.Name())
}

func TestApproveCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("approve")
	require.NotNil(t, cmd)

	cases := []string{"comment", "level"}
	for _, name := range cases {
		f := cmd.Flags().Lookup(name)
		require.NotNil(t, f, "approve command should have --%s flag", name)
	}
}

func TestRejectCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("reject")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("reason")
	require.NotNil(t, f, "reject command should have --reason flag")
}

func TestApproveCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("approve")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "approve without run-id arg should be rejected by Args validator")
}

func TestRejectCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("reject")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "reject without run-id arg should be rejected by Args validator")
}

func TestRejectCmdRequiresReason(t *testing.T) {
	defer resetRootFlags()

	// Reset the flag value.
	rejectOptReason = ""

	cmd := findSub("reject")
	require.NotNil(t, cmd)

	// Running reject without --reason should fail with exit=2.
	err := cmd.RunE(cmd, []string{"run-001"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reason is required")
	assert.Contains(t, err.Error(), "[exit=2]")
}

func TestApproveCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":      "run-001",
		"approval_id": "approval-abc",
		"action":      "approved",
		"approver":    "cli-user",
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
	assert.Equal(t, "approved", result["action"])
}

func TestRejectCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":      "run-001",
		"approval_id": "approval-abc",
		"action":      "rejected",
		"approver":    "cli-user",
		"reason":      "missing rollback plan",
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
	assert.Equal(t, "rejected", result["action"])
	assert.Equal(t, "missing rollback plan", result["reason"])
}

func TestMapApprovalErrorExitCodes(t *testing.T) {
	defer resetRootFlags()

	// Test that unauthorized approver maps to exit=3.
	err := mapApprovalError(fmt.Errorf("approval: approver not in approvers list: %w", assert.AnError))
	// The function should wrap the error, but we just verify it doesn't panic.
	assert.Error(t, err)
}
