package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/approval"
)

// gateOptReason holds the value of the --reason flag for `levee gate reject`.
var gateOptReason string

func init() {
	RegisterCommand(newGateCmd())
}

// newGateCmd builds the `levee gate` command group: the human input surface for
// workflow `human` gates.
func newGateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gate",
		Short: "Decide a workflow human gate",
		Long: "Record a human decision for a workflow `human` gate.\n\n" +
			"A human gate blocks its run until a person approves or rejects it. " +
			"This is that person's input surface, addressed by the run id and the " +
			"gate name — the two values the gate logs when it opens.\n\n" +
			"Unlike `levee approve`, deciding a gate does NOT settle the run: the " +
			"run is already running, and the gate is one step inside it. The " +
			"decision unblocks the gate and nothing else.",
	}
	cmd.AddCommand(newGateApproveCmd(), newGateRejectCmd())
	return cmd
}

func newGateApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <run-id> <gate-name>",
		Short: "Approve a human gate so the run may proceed",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return decideGate(args[0], args[1], true, "")
		},
	}
	return cmd
}

func newGateRejectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reject <run-id> <gate-name>",
		Short: "Reject a human gate so the run fails at it",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if gateOptReason == "" {
				return errors.New("--reason is required")
			}
			return decideGate(args[0], args[1], false, gateOptReason)
		},
	}
	cmd.Flags().StringVar(&gateOptReason, "reason", "", "Why the gate is rejected (required)")
	return cmd
}

// decideGate records the decision on the gate's approval record. The approver is
// the local OS actor (currentActor), matching `levee approve`'s trust boundary:
// whoever can run this command against the data directory already holds the
// database, so the CLI layer is the authentication boundary and the actor it
// reports is what the record carries.
func decideGate(runID, gate string, approve bool, reason string) error {
	if runID == "" || gate == "" {
		return errors.New("run id and gate name are required")
	}
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	svc := approval.NewService(newApprovalStoreAdapter(store))
	id := approval.GateApprovalID(runID, gate)
	approver := currentActor()

	if approve {
		if err := svc.Approve(ctx, id, approver); err != nil {
			return mapApprovalError(err)
		}
		fmt.Fprintf(os.Stderr, "gate %q on run %s approved by %s\n", gate, runID, approver)
		return nil
	}
	if err := svc.Reject(ctx, id, approver, reason); err != nil {
		return mapApprovalError(err)
	}
	fmt.Fprintf(os.Stderr, "gate %q on run %s rejected by %s\n", gate, runID, approver)
	return nil
}
