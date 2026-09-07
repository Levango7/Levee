// cmd_plan.go implements `levee plan <run-id>`: the CLI counterpart of
// the PlanChange RPC. Through the shared engine assembly it resolves the
// run's workflow source, validates the requested hosts against the
// inventory, generates the executable plan and persists it as the run's
// plan artifact (unless --dry-run). Applying a change afterwards executes
// exactly this artifact — plan_hash-bound.

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
)

var (
	planOptTargets []string
	planOptDryRun  bool
)

func init() {
	RegisterCommand(newPlanCmd())
}

// newPlanCmd builds the `levee plan <run-id>` sub-command.
func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan <run-id>",
		Short: "Generate and persist the executable plan for a change run",
		Long: "Resolve the run's workflow, validate --targets against the " +
			"inventory and generate the batched execution plan. Unless " +
			"--dry-run is given the plan is persisted on the run and bound " +
			"by plan_hash: apply then executes exactly this artifact. " +
			"Plan generation only reads the store and inventory; it " +
			"contacts no targets.",
		Args: cobra.ExactArgs(1),
		RunE: runPlan,
	}
	cmd.Flags().StringSliceVar(&planOptTargets, "targets", nil, "Target hosts to plan for (required, comma-separated or repeatable)")
	cmd.Flags().BoolVar(&planOptDryRun, "dry-run", false, "Show the plan without persisting it")
	return cmd
}

func runPlan(cmd *cobra.Command, args []string) error {
	planOptRunID := args[0]

	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	svc := newCLIChangeService(store)
	ctx = grpc.ContextWithActor(ctx, currentActor())

	planMsg, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId:    planOptRunID,
		TargetHosts: planOptTargets,
		DryRun:      planOptDryRun,
	})
	if err != nil {
		return fmt.Errorf("plan %s: %w [exit=1]", planOptRunID, err)
	}

	type batchView struct {
		Index          int      `json:"index"`
		Hosts          []string `json:"hosts"`
		MaxConcurrency int32    `json:"max_concurrency"`
	}
	batches := make([]batchView, 0, len(planMsg.GetBatches()))
	for _, b := range planMsg.GetBatches() {
		batches = append(batches, batchView{
			Index:          int(b.GetIndex()),
			Hosts:          b.GetHosts(),
			MaxConcurrency: b.GetMaxConcurrency(),
		})
	}
	output := map[string]any{
		"run_id":       planMsg.GetChangeId(),
		"target_hosts": planMsg.GetTargetHosts(),
		"batches":      batches,
		"impact":       planMsg.GetImpactSummary(),
		"dry_run":      planOptDryRun,
		"plan_persist": !planOptDryRun,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, planOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Plan for run %s: %d batch(es)\n", planMsg.GetChangeId(), len(batches))
	for _, b := range batches {
		fmt.Fprintf(os.Stdout, "  batch %d: %d host(s), max_concurrency=%d\n", b.Index, len(b.Hosts), b.MaxConcurrency)
		for _, h := range b.Hosts {
			fmt.Fprintf(os.Stdout, "    - %s\n", h)
		}
	}
	if impact := planMsg.GetImpactSummary(); impact != "" {
		fmt.Fprintf(os.Stdout, "impact: %s\n", impact)
	}
	if planOptDryRun {
		fmt.Fprintln(os.Stdout, "dry-run: plan NOT persisted (apply will refuse until planned for real)")
	} else {
		fmt.Fprintln(os.Stdout, "plan persisted and hash-bound; approve then apply to execute this exact plan")
	}
	return nil
}
