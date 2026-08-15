package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

// logsOptTarget holds the value of the --target flag for the logs command.
var logsOptTarget string

// logsOptFollow holds the value of the -f flag for the logs command.
var logsOptFollow bool

func init() {
	RegisterCommand(newLogsCmd())
}

// newLogsCmd builds the `levee logs <run-id>` sub-command.
func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "View logs for a change run",
		Long: "View trace logs for a change run. " +
			"Use --target to filter by host. Use -f to follow the log stream " +
			"(polls for new trace records every 2 seconds).",
		Args: cobra.ExactArgs(1),
		RunE: runLogs,
	}
	cmd.Flags().StringVar(&logsOptTarget, "target", "", "Filter logs by target host")
	cmd.Flags().BoolVarP(&logsOptFollow, "follow", "f", false, "Follow log output (poll every 2s)")
	return cmd
}

// runLogs executes the `levee logs <run-id>` command.
func runLogs(cmd *cobra.Command, args []string) error {
	logsOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Verify the run exists.
	run, err := store.GetRun(ctx, logsOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", logsOptRunID)
	}

	// 3. Query trace records.
	if logsOptFollow {
		return runLogsFollow(ctx, store, logsOptRunID)
	}

	return runLogsOnce(ctx, store, logsOptRunID)
}

// runLogsOnce queries and outputs trace records once.
func runLogsOnce(ctx context.Context, store *state.SQLiteStore, runID string) error {
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	// Filter by target if specified.
	if logsOptTarget != "" {
		traces = filterTracesByTarget(traces, logsOptTarget)
	}

	output := buildLogsOutput(runID, traces)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, t := range traces {
			fmt.Fprintln(os.Stdout, t.ID)
		}
		return nil
	}

	printLogsHuman(os.Stdout, output)
	return nil
}

// runLogsFollow follows the log stream by polling for new trace records.
func runLogsFollow(ctx context.Context, store *state.SQLiteStore, runID string) error {
	var lastCount int

	for {
		traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: runID})
		if err != nil {
			return fmt.Errorf("list traces: %w", err)
		}

		if logsOptTarget != "" {
			traces = filterTracesByTarget(traces, logsOptTarget)
		}

		// Print only new records since last poll.
		if len(traces) > lastCount {
			newTraces := traces[lastCount:]
			for _, t := range newTraces {
				printTraceRecord(os.Stdout, t)
			}
			lastCount = len(traces)
		}

		// Check if the run is still active.
		run, err := store.GetRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("get run: %w", err)
		}
		if run == nil || isTerminalStatus(run.Status) {
			break
		}

		// Wait before next poll.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	return nil
}

// buildLogsOutput constructs the output data structure for the logs command.
func buildLogsOutput(runID string, traces []*state.Trace) map[string]any {
	entries := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		entry := map[string]any{
			"id":        t.ID,
			"event":     t.Event,
			"actor":     t.Actor,
			"timestamp": t.Timestamp,
		}
		// Parse detail JSON to extract target.
		var detail map[string]any
		if t.Detail != "" {
			_ = json.Unmarshal([]byte(t.Detail), &detail)
		}
		if detail != nil {
			if target, ok := detail["target"]; ok {
				entry["target"] = target
			}
		}
		entries = append(entries, entry)
	}

	return map[string]any{
		"run_id":  runID,
		"count":   len(entries),
		"entries": entries,
	}
}

// filterTracesByTarget filters trace records by target host. It parses the
// Detail JSON field to extract the target value.
func filterTracesByTarget(traces []*state.Trace, target string) []*state.Trace {
	var filtered []*state.Trace
	for _, t := range traces {
		var detail map[string]any
		if t.Detail != "" {
			_ = json.Unmarshal([]byte(t.Detail), &detail)
		}
		if detail != nil {
			if tv, ok := detail["target"]; ok && fmt.Sprintf("%v", tv) == target {
				filtered = append(filtered, t)
			}
		}
	}
	return filtered
}

// printLogsHuman renders the logs output in a human-readable format.
func printLogsHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Run: %v\n", output["run_id"])
	fmt.Fprintf(w, "Logs (%v entries):\n", output["count"])

	entries, ok := output["entries"].([]map[string]any)
	if !ok {
		return
	}
	for _, e := range entries {
		ts := e["timestamp"]
		fmt.Fprintf(w, "  %v  %v  actor=%v", ts, e["event"], e["actor"])
		if t, ok := e["target"]; ok {
			fmt.Fprintf(w, "  target=%v", t)
		}
		fmt.Fprintln(w)
	}
}

// printTraceRecord prints a single trace record for follow mode.
func printTraceRecord(w io.Writer, t *state.Trace) {
	fmt.Fprintf(w, "%s  %s  actor=%s\n", t.Timestamp.Format(time.RFC3339), t.Event, t.Actor)
}

// isTerminalStatus reports whether the run status is terminal (no further
// changes expected).
func isTerminalStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled" || status == "rolled_back"
}
