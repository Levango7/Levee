package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
)

// traceOptVerify holds the value of the --verify flag for the trace command.
var traceOptVerify bool

func init() {
	RegisterCommand(newTraceCmd())
}

// newTraceCmd builds the `levee trace <run-id>` sub-command.
func newTraceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace <run-id>",
		Short: "Show audit trace with hash chain verification",
		Long: "Display the complete audit trace for a change run. " +
			"With --verify, the hash chain integrity is verified and " +
			"the verification result is included in the output. " +
			"Exit code 6 indicates chain verification failure.",
		Args: cobra.ExactArgs(1),
		RunE: runTrace,
	}
	cmd.Flags().BoolVar(&traceOptVerify, "verify", false, "Verify hash chain integrity")
	return cmd
}

// runTrace executes the `levee trace <run-id>` command.
func runTrace(cmd *cobra.Command, args []string) error {
	traceOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Verify the run exists.
	run, err := store.GetRun(ctx, traceOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", traceOptRunID)
	}

	// 3. List trace records via TraceRecorder.
	recorder, err := audit.NewTraceRecorder(store)
	if err != nil {
		return fmt.Errorf("create trace recorder: %w", err)
	}
	traces, err := recorder.ListByRun(ctx, traceOptRunID)
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	// 4. Optionally verify the hash chain.
	var verifyResult *audit.VerifyResult
	if traceOptVerify {
		verifier, err := audit.NewChainVerifier(store)
		if err != nil {
			return fmt.Errorf("create chain verifier: %w", err)
		}
		vr, verr := verifier.Verify(ctx, traceOptRunID)
		if verr != nil {
			// Operational error (e.g. no traces), not chain failure.
			return fmt.Errorf("verify chain: %w", verr)
		}
		verifyResult = vr

		// Chain verification failed: exit code 6.
		if !vr.Valid {
			output := buildTraceOutput(traceOptRunID, traces, verifyResult)
			if optJSON {
				_ = PrintJSON(os.Stdout, map[string]any{
					"data":  output,
					"meta":  nil,
					"error": map[string]any{"code": 6, "message": "chain verification failed"},
				})
			} else {
				printTraceHuman(os.Stdout, output)
				fmt.Fprintln(os.Stderr, "ERROR: hash chain verification failed")
			}
			return fmt.Errorf("chain verification failed for run %q: %d of %d records tampered [exit=6]",
				traceOptRunID, len(vr.Failures), vr.Count)
		}
	}

	// 5. Output the result.
	output := buildTraceOutput(traceOptRunID, traces, verifyResult)

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, traceOptRunID)
		return nil
	}

	printTraceHuman(os.Stdout, output)
	return nil
}

// buildTraceOutput constructs the output data structure for the trace command.
func buildTraceOutput(runID string, traces []*state.Trace, verifyResult *audit.VerifyResult) map[string]any {
	entries := make([]map[string]any, 0, len(traces))
	for _, t := range traces {
		entry := map[string]any{
			"id":        t.ID,
			"event":     t.Event,
			"actor":     t.Actor,
			"timestamp": t.Timestamp,
			"prev_hash": t.PrevHash,
			"curr_hash": t.CurrHash,
		}
		entries = append(entries, entry)
	}

	output := map[string]any{
		"run_id":  runID,
		"count":   len(entries),
		"entries": entries,
	}

	if verifyResult != nil {
		output["verify"] = map[string]any{
			"valid":    verifyResult.Valid,
			"count":    verifyResult.Count,
			"failures": verifyResult.Failures,
		}
	}

	return output
}

// printTraceHuman renders the trace output in a human-readable format.
func printTraceHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Run: %v\n", output["run_id"])
	fmt.Fprintf(w, "Trace entries (%v):\n", output["count"])

	entries, ok := output["entries"].([]map[string]any)
	if !ok {
		return
	}
	for _, e := range entries {
		fmt.Fprintf(w, "  %v  %v  actor=%v  hash=%v..%v\n",
			e["timestamp"], e["event"], e["actor"],
			shortHash(e["prev_hash"]), shortHash(e["curr_hash"]))
	}

	if v, ok := output["verify"]; ok {
		verifyMap, _ := v.(map[string]any)
		if verifyMap != nil {
			fmt.Fprintf(w, "\nChain verification:\n")
			fmt.Fprintf(w, "  Valid:  %v\n", verifyMap["valid"])
			fmt.Fprintf(w, "  Count:  %v\n", verifyMap["count"])
			if failures, ok := verifyMap["failures"].([]audit.ChainFailure); ok && len(failures) > 0 {
				fmt.Fprintf(w, "  Failures (%d):\n", len(failures))
				for _, f := range failures {
					fmt.Fprintf(w, "    trace=%s  index=%d  type=%s\n", f.TraceID, f.Index, f.Type)
				}
			}
		}
	}
}

// shortHash returns the first 8 characters of a hash string for display.
// If the value is not a string or is empty, it returns "?".
func shortHash(v any) string {
	s, ok := v.(string)
	if !ok || len(s) == 0 {
		return "?"
	}
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
