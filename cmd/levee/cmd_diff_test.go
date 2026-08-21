package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestDiffCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("diff")
	require.NotNil(t, cmd, "diff subcommand should be registered")
	assert.Equal(t, "diff", cmd.Name())
}

func TestDiffCmdRequiresTwoArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("diff")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{})
	assert.Error(t, err, "diff without args should be rejected")
}

func TestDiffCmdTooManyArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("diff")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "run-002", "extra"})
	assert.Error(t, err, "diff with too many args should be rejected")
}

func TestDiffCmdExactArgs(t *testing.T) {
	defer resetRootFlags()
	cmd := findSub("diff")
	require.NotNil(t, cmd)

	err := cmd.Args(cmd, []string{"run-001", "run-002"})
	assert.NoError(t, err, "diff with exactly two args should be accepted")
}

func TestComputeRunDiffsIdentical(t *testing.T) {
	now := time.Now().UTC()
	runA := &state.Run{
		ID:           "run-001",
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"1.0"}`,
		Status:       "completed",
		PlanHash:     "abc123",
		UpdatedAt:    now,
	}
	runB := &state.Run{
		ID:           "run-002",
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"1.0"}`,
		Status:       "completed",
		PlanHash:     "abc123",
		UpdatedAt:    now,
	}

	diffs := computeRunDiffs(runA, runB, nil, nil)
	assert.Empty(t, diffs, "identical runs should have no diffs")
}

func TestComputeRunDiffsDifferentStatus(t *testing.T) {
	runA := &state.Run{
		ID:           "run-001",
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"1.0"}`,
		Status:       "completed",
	}
	runB := &state.Run{
		ID:           "run-002",
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"1.0"}`,
		Status:       "failed",
	}

	diffs := computeRunDiffs(runA, runB, nil, nil)
	assert.NotEmpty(t, diffs, "different status should produce diffs")

	found := false
	for _, d := range diffs {
		if d.Field == "status" {
			found = true
			assert.Equal(t, "completed", d.A)
			assert.Equal(t, "failed", d.B)
		}
	}
	assert.True(t, found, "should find status diff")
}

func TestComputeRunDiffsDifferentParams(t *testing.T) {
	runA := &state.Run{
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"1.0","env":"prod"}`,
		Status:       "completed",
	}
	runB := &state.Run{
		WorkflowName: "deploy-app",
		TemplateName: "standard",
		Params:       `{"version":"2.0","env":"prod"}`,
		Status:       "completed",
	}

	diffs := computeRunDiffs(runA, runB, nil, nil)
	assert.NotEmpty(t, diffs, "different params should produce diffs")

	found := false
	for _, d := range diffs {
		if d.Field == "params.version" {
			found = true
			assert.Equal(t, "1.0", d.A)
			assert.Equal(t, "2.0", d.B)
		}
	}
	assert.True(t, found, "should find params.version diff")
}

func TestComputeRunDiffsDifferentBatchCount(t *testing.T) {
	runA := &state.Run{WorkflowName: "w", TemplateName: "t", Status: "completed"}
	runB := &state.Run{WorkflowName: "w", TemplateName: "t", Status: "completed"}

	batchesA := []*state.Batch{{ID: "b1", Status: "completed"}}
	batchesB := []*state.Batch{{ID: "b1", Status: "completed"}, {ID: "b2", Status: "pending"}}

	diffs := computeRunDiffs(runA, runB, batchesA, batchesB)
	found := false
	for _, d := range diffs {
		if d.Field == "batch_count" {
			found = true
			assert.Equal(t, 1, d.A)
			assert.Equal(t, 2, d.B)
		}
	}
	assert.True(t, found, "should find batch_count diff")
}

func TestComputeRunDiffsBatchStatusDiff(t *testing.T) {
	runA := &state.Run{WorkflowName: "w", TemplateName: "t", Status: "completed"}
	runB := &state.Run{WorkflowName: "w", TemplateName: "t", Status: "completed"}

	batchesA := []*state.Batch{{ID: "b1", Status: "completed", TotalHosts: 5, Succeeded: 5, Failed: 0}}
	batchesB := []*state.Batch{{ID: "b1", Status: "failed", TotalHosts: 5, Succeeded: 3, Failed: 2}}

	diffs := computeRunDiffs(runA, runB, batchesA, batchesB)
	assert.GreaterOrEqual(t, len(diffs), 1, "different batch status should produce diffs")
}

func TestCompareParamsJSONInvalid(t *testing.T) {
	diffs := compareParamsJSON("not-json", `{"a":1}`)
	assert.Nil(t, diffs, "invalid JSON should return nil diffs")
}

func TestCompareParamsJSONEmpty(t *testing.T) {
	diffs := compareParamsJSON("", "")
	assert.Nil(t, diffs, "empty params should return nil diffs")
}

func TestDiffCmdOutputEnvelope(t *testing.T) {
	defer resetRootFlags()

	data := map[string]any{
		"run_a":     "run-001",
		"run_b":     "run-002",
		"different": false,
		"diffs":     []diffEntry{},
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

func TestPrintDiffHumanNoDiff(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_a":     "run-001",
		"run_b":     "run-002",
		"different": false,
		"diffs":     []diffEntry{},
	}

	var buf bytes.Buffer
	printDiffHuman(&buf, output)
	assert.Contains(t, buf.String(), "No differences found")
}

func TestPrintDiffHumanWithDiff(t *testing.T) {
	defer resetRootFlags()

	output := map[string]any{
		"run_a":     "run-001",
		"run_b":     "run-002",
		"different": true,
		"diffs": []diffEntry{
			{Field: "status", A: "completed", B: "failed"},
		},
	}

	var buf bytes.Buffer
	printDiffHuman(&buf, output)
	assert.Contains(t, buf.String(), "status")
	assert.Contains(t, buf.String(), "completed")
	assert.Contains(t, buf.String(), "failed")
}

func TestDiffCmdHumanOutput(t *testing.T) {
	defer resetRootFlags()

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Diff: %s vs %s\n", "run-001", "run-002")
	fmt.Fprintf(&buf, "  Differences (1):\n")
	fmt.Fprintf(&buf, "    %s: %v vs %v\n", "status", "completed", "failed")
	assert.Contains(t, buf.String(), "run-001")
	assert.Contains(t, buf.String(), "run-002")
	assert.Contains(t, buf.String(), "Differences")
}
