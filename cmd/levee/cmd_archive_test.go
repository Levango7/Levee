package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// TestArchiveTraces_StampsMissingChecksums exercises the fixed archive flow
// at logic level: traces without a checksum are updated in place via
// UpdateTraceChecksum (never Append), already-checksummed records are
// skipped, and a second pass finds nothing left to do.
func TestArchiveTraces_StampsMissingChecksums(t *testing.T) {
	ctx := context.Background()
	store, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "archive-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &state.Run{
		ID: "run-arch", WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "completed", CreatedAt: now, UpdatedAt: now, Creator: "tester",
	}))

	mkTrace := func(id, event string) *state.Trace {
		return &state.Trace{
			ID: id, RunID: "run-arch", Event: event, Actor: "tester",
			Detail:    fmt.Sprintf(`{"n":%q}`, id),
			Timestamp: now.Add(time.Duration(len(id)) * time.Second),
		}
	}
	t1 := mkTrace("trace-1", "plan")
	t2 := mkTrace("trace-2", "step_start")
	t3 := mkTrace("trace-3", "batch_end")
	for _, tr := range []*state.Trace{t1, t2, t3} {
		require.NoError(t, store.CreateTrace(ctx, tr))
	}
	// Pre-checksum the second trace (already archived previously).
	preExisting := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, store.UpdateTraceChecksum(ctx, t2.ID, preExisting))

	fresh, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-arch"})
	require.NoError(t, err)
	require.Len(t, fresh, 3)

	archived, failed, skipped, failedIDs := archiveTraces(ctx, store, fresh)
	assert.Equal(t, 2, archived, "the two unchecksummed traces must be updated")
	assert.Equal(t, 1, skipped, "the pre-checksummed trace must be skipped")
	assert.Zero(t, failed)
	assert.Empty(t, failedIDs)

	got1, err := store.GetTrace(ctx, t1.ID)
	require.NoError(t, err)
	assert.Equal(t, audit.ComputeChecksum(got1), got1.CurrHash,
		"stored curr_hash must equal the audit canonical content checksum")

	got2, err := store.GetTrace(ctx, t2.ID)
	require.NoError(t, err)
	assert.Equal(t, preExisting, got2.CurrHash, "existing hashes are never rewritten")

	// Second pass: everything is now skipped.
	archived, failed, skipped, failedIDs = archiveTraces(ctx, store, fresh)
	assert.Equal(t, 0, archived)
	assert.Equal(t, 3, skipped)
	assert.Zero(t, failed)
	assert.Empty(t, failedIDs)
}

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
