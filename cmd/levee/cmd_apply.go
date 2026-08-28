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
		Long: "Mark the run as running (status-only apply in this build: batches, " +
			"verification gates and rollback are executed by the engine when one " +
			"is wired; the standalone CLI marks status only). Use --force to skip " +
			"the approval check.",
		Args: cobra.ExactArgs(1),
		RunE: runApply,
	}
	cmd.Flags().BoolVar(&applyOptForce, "force", false, "Skip approval check and force apply")
	return cmd
}

// runApply executes the `levee apply <run-id>` command.
// Status-only apply: transitions the run to "running" and reports the
// outcome. Full engine integration (ClosureRunner) is not wired into the
// standalone CLI yet, so this command performs no batch execution.
func runApply(cmd *cobra.Command, args []string) error {
	applyOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

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

	// 4. Transition the run to "running" via a compare-and-set on the
	//    current status, so a concurrent apply (or approval decision)
	//    cannot race this one into a bogus double transition.
	now := time.Now().UTC()
	ok, err := store.UpdateRunStatusIf(ctx, run.ID, run.Status, "running", now)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if !ok {
		latest, lerr := store.GetRun(ctx, applyOptRunID)
		if lerr == nil && latest != nil {
			return fmt.Errorf("run %q is in %q state, cannot apply [exit=4]", applyOptRunID, latest.Status)
		}
		return fmt.Errorf("run %q cannot be applied: concurrent status change [exit=4]", applyOptRunID)
	}

	// 5. Output the result.
	output := map[string]any{
		"run_id":       applyOptRunID,
		"status":       "running",
		"applied_at":   now,
		"applied_by":   currentActor(),
		"force":        applyOptForce,
		"engine_wired": false,
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
	fmt.Fprintln(os.Stdout, "WARNING: no execution engine wired in this build; apply is status-only (no batches executed)")
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
