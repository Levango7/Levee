package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// applyOptForce holds the value of the --force flag for the apply command.
var applyOptForce bool

func init() {
	RegisterCommand(newApplyCmd())
}

// newApplyCmd builds the `levee apply <run-id>` sub-command.
func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply <run-id>",
		Short: "Trigger apply for a change run",
		Long: "Trigger the apply phase for a change run. The command performs " +
			"hash verification, creates a pre-apply snapshot, executes batches " +
			"sequentially, runs verification gates, and triggers rollback on " +
			"failure. Use --force to skip the approval check.",
		Args: cobra.ExactArgs(1),
		RunE: runApply,
	}
	cmd.Flags().BoolVar(&applyOptForce, "force", false, "Skip approval check and force apply")
	return cmd
}

// runApply executes the `levee apply <run-id>` command.
// MVP: marks the run status as "running" and outputs confirmation.
// Full engine integration (ClosureRunner) will be wired in a later batch.
func runApply(cmd *cobra.Command, args []string) error {
	applyOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, applyOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", applyOptRunID)
	}

	// 3. Check whether the run is in an applicable state.
	//    Without --force, only "approved" runs may be applied.
	//    With --force, "pending" and "draft" are also allowed.
	if !isApplicableState(run.Status, applyOptForce) {
		return fmt.Errorf("run %q is in %q state, cannot apply [exit=4]", applyOptRunID, run.Status)
	}

	// 4. Transition the run to "running".
	//    In the full implementation this is where ClosureRunner.Run would be
	//    invoked; the MVP simply updates the status and records a trace.
	now := time.Now().UTC()
	run.Status = "running"
	run.UpdatedAt = now
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	// 5. Output the result.
	output := map[string]any{
		"run_id":     applyOptRunID,
		"status":     "running",
		"applied_at": now,
		"applied_by": currentActor(),
		"force":      applyOptForce,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, applyOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s apply triggered by %s\n", applyOptRunID, currentActor())
	return nil
}

// isApplicableState reports whether a run in the given status can be
// transitioned to "running" by the apply command.
func isApplicableState(status string, force bool) bool {
	switch status {
	case "approved":
		return true
	case "pending", "draft":
		return force
	default:
		return false
	}
}
