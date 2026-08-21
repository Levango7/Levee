package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/state"
)

// rollbackOptForce holds the value of the --force flag for the rollback command.
var rollbackOptForce bool

func init() {
	RegisterCommand(newRollbackCmd())
}

// newRollbackCmd builds the `levee rollback <run-id>` sub-command.
func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback <run-id>",
		Short: "Manually trigger a rollback for a change run",
		Long: "Manually trigger a rollback for a change run. " +
			"The rollback walks the executed batches in reverse order and " +
			"invokes the rollback steps declared on each plan step. " +
			"After rollback completes, post-rollback verification is run " +
			"to confirm the target is in a healthy state. " +
			"Only runs in \"running\", \"completed\", or \"failed\" status " +
			"can be rolled back. Use --force to override status checks.",
		Args: cobra.ExactArgs(1),
		RunE: runRollback,
	}
	cmd.Flags().BoolVar(&rollbackOptForce, "force", false, "Force rollback regardless of run status")
	return cmd
}

// runRollback executes the `levee rollback <run-id>` command.
func runRollback(cmd *cobra.Command, args []string) error {
	rollbackOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, rollbackOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", rollbackOptRunID)
	}

	// 3. Check whether the run is roll-backable.
	if !rollbackOptForce && !isRollbackableStatus(run.Status) {
		return fmt.Errorf("run %q is in %q state, cannot rollback [exit=4]", rollbackOptRunID, run.Status)
	}

	// 4. Transition the run to "rolling_back".
	now := time.Now().UTC()
	run.Status = "rolling_back"
	run.UpdatedAt = now
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	// 5. Record an audit entry.
	audit := &state.Audit{
		ID:        newRollbackAuditID(),
		RunID:     rollbackOptRunID,
		Action:    "rollback",
		Actor:     actor,
		Target:    rollbackOptRunID,
		Result:    "triggered",
		Timestamp: now,
	}
	if err := store.CreateAudit(ctx, audit); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write rollback audit: %v\n", err)
	}

	// 6. Output the result.
	output := map[string]any{
		"run_id":       rollbackOptRunID,
		"action":       "rolling_back",
		"actor":        actor,
		"force":        rollbackOptForce,
		"triggered_at": now,
		"post_verify":  "pending",
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, rollbackOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s rollback triggered by %s\n", rollbackOptRunID, actor)
	fmt.Fprintf(os.Stdout, "  Status: rolling_back\n")
	fmt.Fprintf(os.Stdout, "  Post-rollback verification: pending\n")
	return nil
}

// isRollbackableStatus reports whether a run in the given status can be
// rolled back.
func isRollbackableStatus(status string) bool {
	return status == "running" || status == "completed" || status == "failed"
}

// newRollbackAuditID generates a unique audit identifier for rollback actions.
func newRollbackAuditID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("audit-rollback-%d", time.Now().UnixNano())
	}
	return "audit-rollback-" + hex.EncodeToString(b)
}
