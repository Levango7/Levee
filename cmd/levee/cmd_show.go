package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/state"
)

func init() {
	RegisterCommand(newShowCmd())
}

// newShowCmd builds the `levee show` sub-command.
func newShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <run-id>",
		Short: "Show detailed information about a change run",
		Long: "Display detailed information about a change run including " +
			"its plan, batches, step statuses and audit traces.",
		Args: cobra.ExactArgs(1),
		RunE: runShow,
	}
	return cmd
}

// runShow executes the `levee show <run-id>` command.
func runShow(cmd *cobra.Command, args []string) error {
	showOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Get the run.
	run, err := store.GetRun(ctx, showOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found", showOptRunID)
	}

	// 3. Get batches.
	batches, err := store.ListBatches(ctx, state.BatchFilter{RunID: showOptRunID})
	if err != nil {
		return fmt.Errorf("list batches: %w", err)
	}

	// 4. Get steps.
	steps, err := store.ListSteps(ctx, state.StepFilter{RunID: showOptRunID})
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}

	// 5. Get traces.
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: showOptRunID})
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	// 6. Build the output structure.
	batchSummaries := make([]map[string]any, 0, len(batches))
	for _, b := range batches {
		summary := map[string]any{
			"id":          b.ID,
			"batch_no":    b.BatchNo,
			"status":      b.Status,
			"total_hosts": b.TotalHosts,
			"succeeded":   b.Succeeded,
			"failed":      b.Failed,
		}
		batchSummaries = append(batchSummaries, summary)
	}

	stepSummaries := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		summary := map[string]any{
			"id":        s.ID,
			"batch_id":  s.BatchID,
			"host":      s.Host,
			"step_name": s.StepName,
			"action":    s.Action,
			"status":    s.Status,
		}
		stepSummaries = append(stepSummaries, summary)
	}

	traceSummaries := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		summary := map[string]any{
			"id":        t.ID,
			"event":     t.Event,
			"actor":     t.Actor,
			"timestamp": t.Timestamp,
		}
		traceSummaries = append(traceSummaries, summary)
	}

	output := map[string]any{
		"run":     run,
		"batches": batchSummaries,
		"steps":   stepSummaries,
		"traces":  traceSummaries,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, run.ID)
		return nil
	}

	printShowHuman(os.Stdout, run, batchSummaries, stepSummaries, traceSummaries)
	return nil
}

// printShowHuman renders the show output in a human-readable format.
func printShowHuman(w io.Writer, run *state.Run, batches, steps, traces []map[string]any) {
	fmt.Fprintf(w, "Run: %s\n", run.ID)
	fmt.Fprintf(w, "  Workflow:    %s\n", run.WorkflowName)
	fmt.Fprintf(w, "  Template:    %s\n", run.TemplateName)
	fmt.Fprintf(w, "  Status:      %s\n", run.Status)
	fmt.Fprintf(w, "  Approval:    %s\n", run.ApprovalStatus)
	fmt.Fprintf(w, "  Creator:     %s\n", run.Creator)
	fmt.Fprintf(w, "  Created At:  %s\n", run.CreatedAt.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(w, "  Updated At:  %s\n", run.UpdatedAt.Format("2006-01-02 15:04:05"))

	if len(batches) > 0 {
		fmt.Fprintf(w, "\nBatches (%d):\n", len(batches))
		for _, b := range batches {
			fmt.Fprintf(w, "  #%v  status=%v  hosts=%v  ok=%v  fail=%v\n",
				b["batch_no"], b["status"], b["total_hosts"], b["succeeded"], b["failed"])
		}
	}

	if len(steps) > 0 {
		fmt.Fprintf(w, "\nSteps (%d):\n", len(steps))
		for _, s := range steps {
			fmt.Fprintf(w, "  %v  host=%v  action=%v  status=%v\n",
				s["step_name"], s["host"], s["action"], s["status"])
		}
	}

	if len(traces) > 0 {
		fmt.Fprintf(w, "\nTraces (%d):\n", len(traces))
		for _, t := range traces {
			fmt.Fprintf(w, "  %v  actor=%v  at=%v\n",
				t["event"], t["actor"], t["timestamp"])
		}
	}
}
