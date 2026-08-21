package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/state"
)

// approveOptComment holds the value of the --comment flag for the approve command.
var approveOptComment string

// approveOptLevel holds the value of the --level flag for the approve command.
var approveOptLevel string

// rejectOptReason holds the value of the --reason flag for the reject command.
var rejectOptReason string

func init() {
	RegisterCommand(newApproveCmd())
	RegisterCommand(newRejectCmd())
}

// newApproveCmd builds the `levee approve` sub-command.
func newApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a change run",
		Long: "Record an approval decision for the pending approval on a " +
			"change run. When enough approvers have approved (as defined by " +
			"the approval level), the run transitions to approved status.",
		Args: cobra.ExactArgs(1),
		RunE: runApprove,
	}
	cmd.Flags().StringVar(&approveOptComment, "comment", "", "Optional approval comment")
	cmd.Flags().StringVar(&approveOptLevel, "level", "", "Approval level to act on (standard/high/emergency)")
	return cmd
}

// newRejectCmd builds the `levee reject` sub-command.
func newRejectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a change run",
		Long: "Record a rejection decision for the pending approval on a " +
			"change run. A single rejection immediately vetoes the approval " +
			"(one-vote-veto semantics).",
		Args: cobra.ExactArgs(1),
		RunE: runReject,
	}
	cmd.Flags().StringVar(&rejectOptReason, "reason", "", "Rejection reason (required)")
	return cmd
}

// runApprove executes the `levee approve <run-id>` command.
func runApprove(cmd *cobra.Command, args []string) error {
	approveOptRunID := args[0]
	approver := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Find the pending approval for the given run.
	approvalID, err := findPendingApprovalID(ctx, store, approveOptRunID, approveOptLevel)
	if err != nil {
		return err
	}

	// 3. Create the approval service and record the decision.
	svc := approval.NewService(newApprovalStoreAdapter(store))
	if err := svc.Approve(ctx, approvalID, approver); err != nil {
		return mapApprovalError(err)
	}

	// 4. Output the result.
	output := map[string]any{
		"run_id":      approveOptRunID,
		"approval_id": approvalID,
		"action":      "approved",
		"approver":    approver,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, approvalID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s approved by %s\n", approveOptRunID, approver)
	return nil
}

// runReject executes the `levee reject <run-id> --reason ...` command.
func runReject(cmd *cobra.Command, args []string) error {
	rejectOptRunID := args[0]

	if rejectOptReason == "" {
		return fmt.Errorf("--reason is required for reject [exit=2]")
	}

	approver := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Find the pending approval for the given run.
	approvalID, err := findPendingApprovalID(ctx, store, rejectOptRunID, approveOptLevel)
	if err != nil {
		return err
	}

	// 3. Create the approval service and record the rejection.
	svc := approval.NewService(newApprovalStoreAdapter(store))
	if err := svc.Reject(ctx, approvalID, approver, rejectOptReason); err != nil {
		return mapApprovalError(err)
	}

	// 4. Output the result.
	output := map[string]any{
		"run_id":      rejectOptRunID,
		"approval_id": approvalID,
		"action":      "rejected",
		"approver":    approver,
		"reason":      rejectOptReason,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, approvalID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s rejected by %s: %s\n", rejectOptRunID, approver, rejectOptReason)
	return nil
}

// findPendingApprovalID locates the pending approval record for a given run.
// If level is specified, only approvals at that level are considered.
// Returns the approval ID or an error if no pending approval is found.
func findPendingApprovalID(ctx context.Context, store state.Store, runID, level string) (string, error) {
	filter := state.ApprovalFilter{
		RunID:  runID,
		Status: string(approval.StatusPending),
	}
	if level != "" {
		filter.Level = level
	}

	approvals, err := store.ListApprovals(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("list approvals: %w", err)
	}
	if len(approvals) == 0 {
		return "", fmt.Errorf("no pending approval found for run %q", runID)
	}

	// Return the first pending approval. In a multi-level approval chain,
	// the caller should use --level to target a specific level.
	return approvals[0].ID, nil
}

// mapApprovalError translates approval service errors to CLI-friendly messages
// with appropriate exit codes.
func mapApprovalError(err error) error {
	if isApprovalErr(err, approval.ErrUnauthorizedApprover) {
		return fmt.Errorf("approval denied: %w [exit=3]", err)
	}
	if isApprovalErr(err, approval.ErrInvalidTransition) {
		return fmt.Errorf("approval already decided: %w", err)
	}
	if isApprovalErr(err, approval.ErrNotFound) {
		return fmt.Errorf("approval not found: %w", err)
	}
	return fmt.Errorf("approval: %w", err)
}

// isApprovalErr reports whether err wraps the target sentinel error from the
// approval package. It uses errors.Is for proper error chain unwrapping and
// falls back to substring matching for defensively handling edge cases.
func isApprovalErr(err, target error) bool {
	if errors.Is(err, target) {
		return true
	}
	// Defensive fallback: the approval package wraps errors with
	// fmt.Errorf("%w: ..."), but in case the chain is broken, check
	// whether the target's message appears in the error string.
	return strings.Contains(err.Error(), target.Error())
}
