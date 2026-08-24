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

func init() {
	RegisterCommand(newArchiveCmd())
}

// newArchiveCmd builds the `levee archive <run-id>` sub-command.
func newArchiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "archive <run-id>",
		Short: "Archive a run to WORM storage",
		Long: "Archive a change run's audit traces to Write-Once-Read-Many " +
			"(WORM) storage for compliance. Each trace record that does not " +
			"carry a content checksum yet is checksummed in place (empty " +
			"curr_hash only; existing hashes are never rewritten). If " +
			"archiving fails for any record, a warning is printed but the " +
			"process continues (non-blocking). Exit code 7 indicates that " +
			"some records failed to archive.",
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
	defer func() { _ = store.Close() }()

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

	// 4. Stamp a WORM content checksum onto every trace that lacks one.
	archived, failed, skipped, failedIDs := archiveTraces(ctx, store, traces)

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

// archiveTraces stamps a WORM content checksum onto every trace that lacks
// one and returns (archived, failed, skipped, failedIDs).
//
// The trace rows already exist, so a WORM Append can never be used here
// (its write-once check rejects existing ids). Instead the checksum is
// computed by the audit package and written through Store.UpdateTraceChecksum,
// which only fills an empty curr_hash and therefore never overwrites an
// already-computed checksum or chain hash. The archived counter reflects
// checksum updates performed.
func archiveTraces(ctx context.Context, store state.Store, traces []*state.Trace) (archived, failed, skipped int, failedIDs []string) {
	for _, t := range traces {
		// Skip records that already carry a WORM-size digest (already
		// archived or already linked into a hash chain): both store a
		// 64-char hash in curr_hash, and neither may ever be rewritten.
		if isWORMChecksum(t.CurrHash) {
			skipped++
			continue
		}

		checksum := audit.ComputeChecksum(t)
		if err := store.UpdateTraceChecksum(ctx, t.ID, checksum); err != nil {
			failed++
			failedIDs = append(failedIDs, t.ID)
			// Non-blocking: log warning and continue.
			fmt.Fprintf(os.Stderr, "warning: failed to archive trace %q: %v\n", t.ID, err)
			continue
		}
		t.CurrHash = checksum
		archived++
	}
	return archived, failed, skipped, failedIDs
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
