package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// applyOptForce skips the approval check.
var applyOptForce bool

// applyOptEngineEnabled routes the apply through the assembled execution
// engine (same wiring serve uses). Off by default: the command stays a
// status-only marker, matching the historic CLI behaviour.
var (
	applyOptEngineEnabled  bool
	applyOptMaxConcurrency int32
)

func init() {
	RegisterCommand(newApplyCmd())
}

// newApplyCmd builds the `levee apply <run-id>` sub-command.
func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply <run-id>",
		Short: "Trigger apply for a change run",
		Long: "Apply a change run. Without --engine-enabled this is a " +
			"status-only marker (run -> running; no batches execute). With " +
			"--engine-enabled the command routes the apply through the " +
			"execution engine (same assembly `levee serve --engine-enabled` " +
			"uses): the run must already carry a persisted plan (see " +
			"`levee plan`), which is then executed for real through the " +
			"inventory channels, with batch/step evidence persisted and " +
			"automatic rollback on failure. Use --force to skip the " +
			"approval check.",
		Args: cobra.ExactArgs(1),
		RunE: runApply,
	}
	cmd.Flags().BoolVar(&applyOptForce, "force", false, "Skip approval check and force apply")
	cmd.Flags().BoolVar(&applyOptEngineEnabled, "engine-enabled", false, "Execute the run for real through the assembled engine (requires a persisted plan; set LEVEE_MASTER_PASSWORD for credentialed channels)")
	cmd.Flags().Int32Var(&applyOptMaxConcurrency, "max-concurrency", 0, "Override the plan's per-batch max concurrency (engine mode only; 0 = use the plan)")
	return cmd
}

// runApply executes the `levee apply <run-id>` command.
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

	// 4a. Engine mode: delegate the whole apply (plan-artifact gates, CAS,
	// execution, terminal status, audit) to the in-process ChangeService,
	// so CLI and serve enforce identical rules.
	if applyOptEngineEnabled {
		return runApplyWithEngine(ctx, store, run)
	}

	// 4b. Status-only mode (default): transition the run to "running" via a
	//     compare-and-set on the current status, so a concurrent apply (or
	//     approval decision) cannot race this one into a bogus double
	//     transition.
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

// runApplyWithEngine executes the run for real through the assembled
// engine via the in-process ChangeService. The service re-checks the
// approval state (honouring --force as auto-approve), refuses runs without
// a persisted plan artifact (run `levee plan` first), CAS-es into
// "running", drives the closure synchronously and writes the terminal
// status; this function only maps the outcome to CLI conventions.
func runApplyWithEngine(ctx context.Context, store *state.SQLiteStore, run *state.Run) error {
	svc := newCLIChangeService(store)
	ctx = grpc.ContextWithActor(ctx, currentActor())

	resp, err := svc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId:       run.ID,
		AutoApprove:    applyOptForce,
		MaxConcurrency: applyOptMaxConcurrency,
	})
	if err != nil {
		// FailedPrecondition = a gate refusal (state/approval/plan artifact):
		// same CLI exit as the status-only state guard. Anything else is an
		// execution/store error.
		exit := 1
		if grpcstatus.Code(err) == codes.FailedPrecondition {
			exit = 4
		}
		return fmt.Errorf("apply %s: %w [exit=%d]", run.ID, err, exit)
	}

	finalStatus := run2Status(resp)
	output := map[string]any{
		"run_id":         run.ID,
		"status":         finalStatus,
		"applied_at":     time.Now().UTC(),
		"applied_by":     currentActor(),
		"force":          applyOptForce,
		"engine_wired":   true,
		"exec_run_id":    resp.GetRunId(),
		"success":        resp.GetSuccess(),
		"engine_message": resp.GetMessage(),
	}

	if optJSON {
		_ = PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
		if !resp.GetSuccess() {
			return fmt.Errorf("run %s finished in %q state [exit=1]", run.ID, finalStatus)
		}
		return nil
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, run.ID)
	} else {
		fmt.Fprintf(os.Stdout, "Run %s executed by engine: %s\n", run.ID, finalStatus)
		if resp.GetRunId() != "" {
			fmt.Fprintf(os.Stdout, "  engine run id: %s\n", resp.GetRunId())
		}
	}
	if !resp.GetSuccess() {
		return fmt.Errorf("run %s finished in %q state [exit=1]", run.ID, finalStatus)
	}
	return nil
}

// run2Status extracts the run status from an ApplyResponse, tolerating a
// nil Change message.
func run2Status(resp *pb.ApplyResponse) string {
	if c := resp.GetChange(); c != nil {
		return c.GetStatus()
	}
	return ""
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
