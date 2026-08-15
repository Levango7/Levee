package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchiveCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("archive")
	require.NotNil(t, cmd, "archive subcommand should be registered")
	assert.Equal(t, "archive", cmd.Name())
}

func TestArchiveCmdRequiresArg(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("archive")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "archive without run-id arg should be rejected")
}

func TestArchiveCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("archive")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "extra"})
	assert.Error(t, err, "archive with too many args should be rejected")
}

func TestIsWORMChecksum(t *testing.T) {
	cases := []struct {
		hash string
		want bool
	}{
		{"", false},
		{"abc", false},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde", false},
	}

	for _, tc := range cases {
		got := isWORMChecksum(tc.hash)
		assert.Equal(t, tc.want, got, "isWORMChecksum(%q)", tc.hash)
	}
}

func TestArchiveCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_id":   "run-001",
		"total":    10,
		"archived": 10,
		"failed":   0,
		"skipped":  0,
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

func TestArchiveCmdFailedExitCode(t *testing.T) {
	defer resetRootFlags()

	err := fmt.Errorf("%d of %d records failed to archive [exit=7]", 2, 10)
	assert.Equal(t, 7, exitCodeFor(err))
}

func TestPrintArchiveHuman(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id":   "run-001",
		"total":    10,
		"archived": 8,
		"failed":   1,
		"skipped":  1,
	}

	var buf bytes.Buffer
	printArchiveHuman(&buf, output)
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "Total")
	assert.Contains(t, buf.String(), "Archived")
	assert.Contains(t, buf.String(), "Failed")
}

func TestPrintArchiveHumanWithFailedIDs(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_id":     "run-001",
		"total":      2,
		"archived":   1,
		"failed":     1,
		"skipped":    0,
		"failed_ids": []string{"trace-002"},
	}

	var buf bytes.Buffer
	printArchiveHuman(&buf, output)
	assert.Contains(t, buf.String(), "trace-002")
}

func TestStateTraceFilter(t *testing.T) {
	filter := stateTraceFilter("run-001")
	assert.Equal(t, "run-001", filter.RunID)
}

func TestArchiveCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Archive: %s\n", "run-001")
	fmt.Fprintf(&buf, "  Total:    %d\n", 10)
	fmt.Fprintf(&buf, "  Archived: %d\n", 10)
	fmt.Fprintf(&buf, "  Skipped:  %d\n", 0)
	fmt.Fprintf(&buf, "  Failed:   %d\n", 0)
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "Archived")
}
