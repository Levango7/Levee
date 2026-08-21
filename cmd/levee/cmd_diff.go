package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/state"
)

func init() {
	RegisterCommand(newDiffCmd())
}

// newDiffCmd builds the `levee diff <run-a> <run-b>` sub-command.
func newDiffCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diff <run-a> <run-b>",
		Short: "Compare two change runs",
		Long: "Compare two change runs by their parameters, status, " +
			"and batch execution plans. Outputs the differences between " +
			"the two runs. Exit code 0 is returned whether or not " +
			"differences exist; exit code 1 indicates an operational error.",
		Args: cobra.ExactArgs(2),
		RunE: runDiff,
	}
	return cmd
}

// runDiff executes the `levee diff <run-a> <run-b>` command.
func runDiff(cmd *cobra.Command, args []string) error {
	diffOptRunA := args[0]
	diffOptRunB := args[1]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Load both runs.
	runA, err := store.GetRun(ctx, diffOptRunA)
	if err != nil {
		return fmt.Errorf("get run %q: %w", diffOptRunA, err)
	}
	if runA == nil {
		return fmt.Errorf("run %q not found [exit=1]", diffOptRunA)
	}

	runB, err := store.GetRun(ctx, diffOptRunB)
	if err != nil {
		return fmt.Errorf("get run %q: %w", diffOptRunB, err)
	}
	if runB == nil {
		return fmt.Errorf("run %q not found [exit=1]", diffOptRunB)
	}

	// 3. Load batches for both runs.
	batchesA, err := store.ListBatches(ctx, state.BatchFilter{RunID: diffOptRunA})
	if err != nil {
		return fmt.Errorf("list batches for %q: %w", diffOptRunA, err)
	}
	batchesB, err := store.ListBatches(ctx, state.BatchFilter{RunID: diffOptRunB})
	if err != nil {
		return fmt.Errorf("list batches for %q: %w", diffOptRunB, err)
	}

	// 4. Compute differences.
	diffs := computeRunDiffs(runA, runB, batchesA, batchesB)

	// 5. Output the result.
	output := map[string]any{
		"run_a":     diffOptRunA,
		"run_b":     diffOptRunB,
		"different": len(diffs) > 0,
		"diffs":     diffs,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		if len(diffs) > 0 {
			fmt.Fprintln(os.Stdout, "different")
		} else {
			fmt.Fprintln(os.Stdout, "identical")
		}
		return nil
	}

	printDiffHuman(os.Stdout, output)
	return nil
}

// diffEntry represents a single difference between two runs.
type diffEntry struct {
	Field string `json:"field"`
	A     any    `json:"a"`
	B     any    `json:"b"`
}

// computeRunDiffs compares two runs and their batches, returning the
// list of differences.
func computeRunDiffs(runA, runB *state.Run, batchesA, batchesB []*state.Batch) []diffEntry {
	var diffs []diffEntry

	// Compare run-level fields.
	if runA.WorkflowName != runB.WorkflowName {
		diffs = append(diffs, diffEntry{Field: "workflow_name", A: runA.WorkflowName, B: runB.WorkflowName})
	}
	if runA.TemplateName != runB.TemplateName {
		diffs = append(diffs, diffEntry{Field: "template_name", A: runA.TemplateName, B: runB.TemplateName})
	}
	if runA.Params != runB.Params {
		diffs = append(diffs, diffEntry{Field: "params", A: runA.Params, B: runB.Params})
	}
	if runA.Status != runB.Status {
		diffs = append(diffs, diffEntry{Field: "status", A: runA.Status, B: runB.Status})
	}
	if runA.PlanHash != runB.PlanHash {
		diffs = append(diffs, diffEntry{Field: "plan_hash", A: runA.PlanHash, B: runB.PlanHash})
	}
	if runA.ApprovalStatus != runB.ApprovalStatus {
		diffs = append(diffs, diffEntry{Field: "approval_status", A: runA.ApprovalStatus, B: runB.ApprovalStatus})
	}
	if runA.ApprovalLevel != runB.ApprovalLevel {
		diffs = append(diffs, diffEntry{Field: "approval_level", A: runA.ApprovalLevel, B: runB.ApprovalLevel})
	}
	if runA.IncidentID != runB.IncidentID {
		diffs = append(diffs, diffEntry{Field: "incident_id", A: runA.IncidentID, B: runB.IncidentID})
	}

	// Compare batch counts.
	if len(batchesA) != len(batchesB) {
		diffs = append(diffs, diffEntry{Field: "batch_count", A: len(batchesA), B: len(batchesB)})
	}

	// Compare individual batches.
	minLen := len(batchesA)
	if len(batchesB) < minLen {
		minLen = len(batchesB)
	}
	for i := 0; i < minLen; i++ {
		prefix := fmt.Sprintf("batch[%d]", i)
		if batchesA[i].Status != batchesB[i].Status {
			diffs = append(diffs, diffEntry{Field: prefix + ".status", A: batchesA[i].Status, B: batchesB[i].Status})
		}
		if batchesA[i].TotalHosts != batchesB[i].TotalHosts {
			diffs = append(diffs, diffEntry{Field: prefix + ".total_hosts", A: batchesA[i].TotalHosts, B: batchesB[i].TotalHosts})
		}
		if batchesA[i].Succeeded != batchesB[i].Succeeded {
			diffs = append(diffs, diffEntry{Field: prefix + ".succeeded", A: batchesA[i].Succeeded, B: batchesB[i].Succeeded})
		}
		if batchesA[i].Failed != batchesB[i].Failed {
			diffs = append(diffs, diffEntry{Field: prefix + ".failed", A: batchesA[i].Failed, B: batchesB[i].Failed})
		}
	}

	// Compare params as structured JSON when possible.
	diffs = append(diffs, compareParamsJSON(runA.Params, runB.Params)...)

	return diffs
}

// compareParamsJSON attempts to parse both param strings as JSON and
// compare them field by field. If either is not valid JSON, it falls
// back to the string comparison already done in computeRunDiffs.
func compareParamsJSON(paramsA, paramsB string) []diffEntry {
	if paramsA == "" || paramsB == "" {
		return nil
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(paramsA), &a); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(paramsB), &b); err != nil {
		return nil
	}

	var diffs []diffEntry

	// Check keys present in a.
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			diffs = append(diffs, diffEntry{Field: "params." + k, A: va, B: nil})
			continue
		}
		if fmt.Sprintf("%v", va) != fmt.Sprintf("%v", vb) {
			diffs = append(diffs, diffEntry{Field: "params." + k, A: va, B: vb})
		}
	}

	// Check keys only in b.
	for k, vb := range b {
		if _, ok := a[k]; !ok {
			diffs = append(diffs, diffEntry{Field: "params." + k, A: nil, B: vb})
		}
	}

	return diffs
}

// printDiffHuman renders the diff output in a human-readable format.
func printDiffHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Diff: %v vs %v\n", output["run_a"], output["run_b"])

	different, _ := output["different"].(bool)
	if !different {
		fmt.Fprintln(w, "  No differences found.")
		return
	}

	diffs, ok := output["diffs"].([]diffEntry)
	if !ok {
		return
	}
	fmt.Fprintf(w, "  Differences (%d):\n", len(diffs))
	for _, d := range diffs {
		fmt.Fprintf(w, "    %s: %v vs %v\n", d.Field, d.A, d.B)
	}
}
