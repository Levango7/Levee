package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

// cancelOptReason holds the value of the --reason flag for the cancel command.
var cancelOptReason string

func init() {
	RegisterCommand(newCancelCmd())
}

// newCancelCmd builds the `levee cancel <run-id>` sub-command.
func newCancelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a change run",
		Long: "Cancel a change run that has not yet started execution. " +
			"Only runs in \"pending\" or \"draft\" status can be cancelled. " +
			"Running or completed runs cannot be cancelled; use pause or " +
			"rollback instead. An audit entry is recorded with the reason.",
		Args: cobra.ExactArgs(1),
		RunE: runCancel,
	}
	cmd.Flags().StringVar(&cancelOptReason, "reason", "", "Reason for cancellation (recorded in audit)")
	return cmd
}

// runCancel executes the `levee cancel <run-id>` command.
func runCancel(cmd *cobra.Command, args []string) error {
	cancelOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, cancelOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", cancelOptRunID)
	}

	// 3. Check whether the run is cancellable.
	//    Only "pending" and "draft" runs can be cancelled.
	if !isCancellableStatus(run.Status) {
		return fmt.Errorf("run %q is in %q state, cannot cancel [exit=4]", cancelOptRunID, run.Status)
	}

	// 4. Transition the run to "cancelled".
	now := time.Now().UTC()
	run.Status = "cancelled"
	run.UpdatedAt = now
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	// 5. Record an audit entry.
	audit := &state.Audit{
		ID:        newCancelAuditID(),
		RunID:     cancelOptRunID,
		Action:    "cancel",
		Actor:     actor,
		Target:    cancelOptRunID,
		Result:    "success",
		Timestamp: now,
	}
	if err := store.CreateAudit(ctx, audit); err != nil {
		// Audit write failure is observability-only: the state transition
		// has already been persisted, so we log and continue.
		fmt.Fprintf(os.Stderr, "warning: failed to write cancel audit: %v\n", err)
	}

	// 6. Output the result.
	output := map[string]any{
		"run_id":       cancelOptRunID,
		"action":       "cancelled",
		"actor":        actor,
		"reason":       cancelOptReason,
		"cancelled_at": now,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, cancelOptRunID)
		return nil
	}

	reason := cancelOptReason
	if reason != "" {
		reason = ": " + reason
	}
	fmt.Fprintf(os.Stdout, "Run %s cancelled by %s%s\n", cancelOptRunID, actor, reason)
	return nil
}

// isCancellableStatus reports whether a run in the given status can be
// cancelled.
func isCancellableStatus(status string) bool {
	return status == "pending" || status == "draft"
}

// newCancelAuditID generates a unique audit identifier for cancel actions.
func newCancelAuditID() string {
	b := make([]byte, 8)
	// Best-effort random; on error fall back to timestamp.
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("audit-cancel-%d", time.Now().UnixNano())
	}
	return "audit-cancel-" + hex.EncodeToString(b)
}
