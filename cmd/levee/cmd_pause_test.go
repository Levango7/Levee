package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/pause"
)

func TestPauseCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause")
	require.NotNil(t, cmd, "pause subcommand should be registered")
	assert.Equal(t, "pause", cmd.Name())
}

func TestResumeCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("resume")
	require.NotNil(t, cmd, "resume subcommand should be registered")
	assert.Equal(t, "resume", cmd.Name())
}

func TestPauseAllCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause-all")
	require.NotNil(t, cmd, "pause-all subcommand should be registered")
	assert.Equal(t, "pause-all", cmd.Name())
}

func TestResumeAllCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("resume-all")
	require.NotNil(t, cmd, "resume-all subcommand should be registered")
	assert.Equal(t, "resume-all", cmd.Name())
}

func TestPauseAllCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause-all")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("reason")
	require.NotNil(t, f, "pause-all command should have --reason flag")
}

func TestResumeAllCmdFlags(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("resume-all")
	require.NotNil(t, cmd)

	f := cmd.Flags().Lookup("reason")
	require.NotNil(t, f, "resume-all command should have --reason flag")
}

func TestPauseCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "pause without run-id arg should be rejected")
}

func TestResumeCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("resume")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "resume without run-id arg should be rejected")
}

func TestPauseAllCmdNoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause-all")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.NoError(t, err, "pause-all accepts no args")
}

func TestPauseAllCmdRejectsArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("pause-all")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"extra"})
	assert.Error(t, err, "pause-all should reject extra args")
}

func TestPauseCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id": "run-001",
		"action": "paused",
		"actor":  "cli-user",
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
	assert.Equal(t, "paused", result["action"])
	assert.Equal(t, "run-001", result["run_id"])
}

func TestResumeCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id": "run-001",
		"action": "resumed",
		"actor":  "cli-user",
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
	assert.Equal(t, "resumed", result["action"])
}

func TestMapPauseErrorExitCodes(t *testing.T) {
	cases := []struct {
		err      error
		contains string
		exitCode int
	}{
		{pause.ErrRunNotFound, "run not found", 1},
		{pause.ErrNotPausable, "cannot pause", 1},
		{pause.ErrNotResumable, "cannot resume", 1},
		{pause.ErrPermissionDenied, "permission denied", 3},
		{pause.ErrEmptyRunID, "invalid input", 2},
		{pause.ErrEmptyActor, "invalid input", 2},
	}

	for _, tc := range cases {
		mapped := mapPauseError(tc.err)
		assert.Contains(t, mapped.Error(), tc.contains, "mapPauseError(%v)", tc.err)
		assert.Equal(t, tc.exitCode, exitCodeFor(mapped), "exitCodeFor(mapPauseError(%v))", tc.err)
	}
}

func TestMapPauseErrorUnknown(t *testing.T) {
	err := errors.New("something unexpected")
	mapped := mapPauseError(err)
	assert.Contains(t, mapped.Error(), "pause:")
	assert.Equal(t, 1, exitCodeFor(mapped))
}

func TestNewCLIPermissionCheckerDefault(t *testing.T) {
	// Without LEVEE_PERMISSIONS env, the checker should grant all.
	checker := newCLIPermissionChecker()
	actor := currentActor()
	assert.True(t, checker.HasPermission(actor, pause.PermissionPauseAll))
	assert.True(t, checker.HasPermission(actor, pause.PermissionResumeAll))
}

func TestNewCLIPermissionCheckerRestricted(t *testing.T) {
	// Set env to only grant pause:all.
	t.Setenv("LEVEE_PERMISSIONS", "pause:all")
	checker := newCLIPermissionChecker()
	actor := currentActor()
	assert.True(t, checker.HasPermission(actor, pause.PermissionPauseAll))
	assert.False(t, checker.HasPermission(actor, pause.PermissionResumeAll))
}

func TestFormatFailedMapNil(t *testing.T) {
	result := formatFailedMap(nil)
	assert.Nil(t, result)
}

func TestFormatFailedMapEmpty(t *testing.T) {
	result := formatFailedMap(map[string]error{})
	assert.Nil(t, result)
}

func TestFormatFailedMapWithErrors(t *testing.T) {
	failed := map[string]error{
		"run-001": errors.New("update failed"),
		"run-002": errors.New("state conflict"),
	}
	result := formatFailedMap(failed)
	assert.Equal(t, 2, len(result))
	assert.Equal(t, "update failed", result["run-001"])
	assert.Equal(t, "state conflict", result["run-002"])
}

func TestPauseAllOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"action":   "pause_all",
		"actor":    "cli-user",
		"reason":   "maintenance window",
		"affected": []string{"run-001", "run-002"},
		"failed":   nil,
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
	assert.Equal(t, "pause_all", result["action"])
}
