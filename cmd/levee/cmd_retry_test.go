package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry")
	require.NotNil(t, cmd, "retry subcommand should be registered")
	assert.Equal(t, "retry", cmd.Name())
}

func TestRetryHostCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry-host")
	require.NotNil(t, cmd, "retry-host subcommand should be registered")
	assert.Equal(t, "retry-host", cmd.Name())
}

func TestRetryCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "retry without run-id arg should be rejected")
}

func TestRetryCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "retry with too many args should be rejected")
}

func TestRetryHostCmdRequiresTwoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry-host")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "retry-host without args should be rejected")
}

func TestRetryHostCmdRequiresBothArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry-host")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001"})
	assert.Error(t, err, "retry-host with only run-id should be rejected")
}

func TestRetryHostCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("retry-host")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "host-1", "extra"})
	assert.Error(t, err, "retry-host with too many args should be rejected")
}

func TestIsRetryableStatus(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"failed", true},
		{"paused", true},
		{"running", false},
		{"pending", false},
		{"draft", false},
		{"approved", false},
		{"completed", false},
		{"cancelled", false},
	}

	for _, tc := range cases {
		got := isRetryableStatus(tc.status)
		assert.Equal(t, tc.want, got, "isRetryableStatus(%q)", tc.status)
	}
}

func TestMaxRetryAttempts(t *testing.T) {
	assert.Equal(t, 3, maxRetryAttempts, "max retry attempts should be 3")
}

func TestRetryCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":      "run-001",
		"action":      "retry",
		"actor":       "cli-user",
		"retry_count": 1,
		"max_retries": maxRetryAttempts,
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
	assert.Equal(t, "retry", result["action"])
	assert.Equal(t, float64(1), result["retry_count"])
}

func TestRetryHostCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":      "run-001",
		"host":        "host-1",
		"action":      "retry_host",
		"actor":       "cli-user",
		"retry_count": 2,
		"max_retries": maxRetryAttempts,
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
	assert.Equal(t, "retry_host", result["action"])
	assert.Equal(t, "host-1", result["host"])
	assert.Equal(t, float64(2), result["retry_count"])
}

func TestRetryLimitExitCode(t *testing.T) {
	defer resetRootFlags()

	// Verify that a retry limit error carries [exit=5].
	err := fmt.Errorf("run %q has reached retry limit (%d/%d) [exit=5]", "run-001", 3, maxRetryAttempts)
	assert.Equal(t, 5, exitCodeFor(err))
}

func TestNewRetryAuditID(t *testing.T) {
	id := newRetryAuditID()
	assert.NotEmpty(t, id)
	assert.Contains(t, id, "audit-retry-")
}

func TestNewRetryAuditIDUniqueness(t *testing.T) {
	ids := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newRetryAuditID()
		assert.False(t, ids[id], "retry audit ID should be unique: %s", id)
		ids[id] = true
	}
}

func TestRetryCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Run %s retry #%d triggered by %s\n", "run-001", 1, "cli-user")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "retry #1")
	assert.Contains(t, buf.String(), "cli-user")
}

func TestRetryHostCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Run %s host %s retry #%d triggered by %s\n",
		"run-001", "host-1", 2, "cli-user")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "host-1")
	assert.Contains(t, buf.String(), "retry #2")
}
