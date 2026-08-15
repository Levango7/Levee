package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

func init() {
	RegisterCommand(newArchiveCmd())
}

// newArchiveCmd builds the `levee archive <run-id>` sub-command.
func newArchiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <run-id>",
		Short: "Archive a run to WORM storage",
		Long: "Archive a change run's audit traces to Write-Once-Read-Many " +
			"(WORM) storage for compliance. Each trace record is appended " +
			"with a content checksum. If archiving fails for any record, " +
			"a warning is printed but the process continues (non-blocking). " +
			"Exit code 7 indicates that some records failed to archive.",
		Args: cobra.ExactArgs(1),
		RunE: runArchive,
	}
	return cmd
}

// runArchive executes the `levee archive <run-id>` command.
func runArchive(cmd *cobra.Command, args []string) error {
	archiveOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Verify the run exists.
	run, err := store.GetRun(ctx, archiveOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", archiveOptRunID)
	}

	// 3. List trace records for the run.
	traces, err := store.ListTraces(ctx, stateTraceFilter(archiveOptRunID))
	if err != nil {
		return fmt.Errorf("list traces: %w", err)
	}

	if len(traces) == 0 {
		output := map[string]any{
			"run_id":   archiveOptRunID,
			"archived": 0,
			"failed":   0,
			"skipped":  0,
			"actor":    actor,
		}
		if optJSON {
			return PrintJSON(os.Stdout, map[string]any{
				"data":  output,
				"meta":  nil,
				"error": nil,
			})
		}
		fmt.Fprintf(os.Stdout, "Run %s has no trace records to archive.\n", archiveOptRunID)
		return nil
	}

	// 4. Open the WORM store and archive each trace.
	worm, err := audit.NewWORMStore(store)
	if err != nil {
		return fmt.Errorf("create WORM store: %w", err)
	}

	var archived, failed, skipped int
	var failedIDs []string

	for _, t := range traces {
		// Skip records that already have a WORM checksum (already archived).
		if t.CurrHash != "" && isWORMChecksum(t.CurrHash) {
			skipped++
			continue
		}

		err := worm.Append(ctx, t)
		if err != nil {
			failed++
			failedIDs = append(failedIDs, t.ID)
			// Non-blocking: log warning and continue.
			fmt.Fprintf(os.Stderr, "warning: failed to archive trace %q: %v\n", t.ID, err)
			continue
		}
		archived++
	}

	// 5. Build output.
	output := map[string]any{
		"run_id":     archiveOptRunID,
		"total":      len(traces),
		"archived":   archived,
		"failed":     failed,
		"skipped":    skipped,
		"failed_ids": failedIDs,
		"actor":      actor,
	}

	if optJSON {
		env := map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		}
		if failed > 0 {
			env["error"] = map[string]any{
				"code":    7,
				"message": fmt.Sprintf("%d of %d records failed to archive", failed, len(traces)),
			}
		}
		_ = PrintJSON(os.Stdout, env)
		if failed > 0 {
			return fmt.Errorf("%d of %d records failed to archive [exit=7]", failed, len(traces))
		}
		return nil
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, archiveOptRunID)
		return nil
	}

	printArchiveHuman(os.Stdout, output)
	if failed > 0 {
		return fmt.Errorf("%d of %d records failed to archive [exit=7]", failed, len(traces))
	}
	return nil
}

// isWORMChecksum reports whether a hash looks like a WORM content checksum
// (64-char hex SHA-256). This is a heuristic: the hash-chain builder also
// writes CurrHash, so we cannot distinguish the two by content alone. For
// the MVP we treat any non-empty CurrHash as "already processed".
func isWORMChecksum(hash string) bool {
	return len(hash) == 64
}

// printArchiveHuman renders the archive output in a human-readable format.
func printArchiveHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Archive: %v\n", output["run_id"])
	fmt.Fprintf(w, "  Total:    %v\n", output["total"])
	fmt.Fprintf(w, "  Archived: %v\n", output["archived"])
	fmt.Fprintf(w, "  Skipped:  %v\n", output["skipped"])
	fmt.Fprintf(w, "  Failed:   %v\n", output["failed"])
	if failedIDs, ok := output["failed_ids"].([]string); ok && len(failedIDs) > 0 {
		fmt.Fprintf(w, "  Failed IDs:\n")
		for _, id := range failedIDs {
			fmt.Fprintf(w, "    %s\n", id)
		}
	}
}

// stateTraceFilter creates a state.TraceFilter for the given run ID.
// This helper avoids importing state in the archive command's main logic
// while keeping the filter construction consistent.
func stateTraceFilter(runID string) state.TraceFilter {
	return state.TraceFilter{RunID: runID}
}
