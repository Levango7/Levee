package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/runstatus"
)

// rollbackOptDryRun holds the value of the --dry-run flag: preview the
// rollback instead of performing it.
var rollbackOptDryRun bool

func init() {
	RegisterCommand(newRollbackCmd())
}

// newRollbackCmd builds the `levee rollback <run-id>` sub-command.
//
// The command delegates to ChangeService.RollbackChange — the same service the
// server exposes — through the CLI's engine bridge, so the CLI and the
// console enforce the same gates and leave the same evidence.
//
// It used to be a local store write. `levee rollback <run-id>` set the run's
// status to "rolling_back", wrote an audit row, printed "rollback triggered"
// and stopped: no compensation ran, and "rolling_back" had no consumer
// anywhere in the repository (it was not in the run status vocabulary, no
// dispatcher or worker looked for it, and `levee logs` waits for a terminal
// status — so the run it left behind was stranded with no command able to move
// it out). The command claimed to roll a change back and in fact did nothing
// but strand the change.
func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback <run-id>",
		Short: "Roll back a change run",
		Long: "Roll back a change run through the same service the server uses. " +
			"The rollback walks the executed batches in reverse order and invokes " +
			"the compensation declared on each executed step; the run is then " +
			"marked with the rollback verdict (rolled_back, rolled_back_partial or " +
			"rollback_incomplete). " +
			"Use --dry-run to preview: a preview compensates no host, rewrites no " +
			"record and writes no audit row — it reports which hosts would be " +
			"compensated and which would be left alone. " +
			// The admitted set is rendered from runstatus so this help text
			// cannot contradict the server guard (ChangeService.RollbackChange
			// admits exactly these).
			"Only runs in " + runstatus.JoinRollbackAdmitted() + " status can be rolled back.",
		Args: cobra.ExactArgs(1),
		RunE: runRollback,
	}
	cmd.Flags().BoolVar(&rollbackOptDryRun, "dry-run", false,
		"Preview the rollback: report what it would do, without compensating any host or changing any record")
	return cmd
}

// runRollback executes the `levee rollback <run-id>` command.
func runRollback(cmd *cobra.Command, args []string) error {
	runID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Load the run record.
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found [exit=1]", runID)
	}

	// 3. A run stranded in the legacy "rolling_back" state (written by the
	//    retired CLI, which never executed anything) is admitted by no gate —
	//    not by this command, not by the service. Mark it with the verdict
	//    that is true of it ("a rollback was requested and no compensation
	//    completed") so the operator can actually re-drive it.
	if runstatus.IsLegacyRollingBack(run.Status) {
		if rollbackOptDryRun {
			// A preview persists nothing — not even this repair.
			return fmt.Errorf("run %q is stranded in the legacy %q state the retired CLI wrote without "+
				"executing anything; re-run the rollback without --dry-run to release it [exit=4]",
				runID, runstatus.StatusRollingBackLegacy)
		}
		now := time.Now().UTC()
		ok, err := store.UpdateRunStatusIf(ctx, run.ID, run.Status, runstatus.StatusRollbackIncomplete, now)
		if err != nil {
			return fmt.Errorf("update run: %w", err)
		}
		if !ok {
			return fmt.Errorf("run %q changed state concurrently; re-run the rollback [exit=4]", runID)
		}
		run.Status = runstatus.StatusRollbackIncomplete
		fmt.Fprintf(os.Stderr,
			"warning: run %s was stranded in the legacy %q state (the retired CLI wrote it without "+
				"executing anything); marked it %q so the rollback can run\n",
			runID, runstatus.StatusRollingBackLegacy, runstatus.StatusRollbackIncomplete)
	}

	// 4. Delegate to the service: the admission guard, the risk gate, the
	//    compensation dispatch, the terminal status and the audit row are the
	//    server's, so the CLI cannot drift away from the console's behaviour.
	svc, err := cliChangeServiceFactory(store)
	if err != nil {
		return err
	}
	ctx = grpc.ContextWithActor(ctx, actor)

	resp, err := svc.RollbackChange(ctx, &pb.RollbackRequest{
		ChangeId: runID,
		DryRun:   rollbackOptDryRun,
	})
	if err != nil {
		return fmt.Errorf("rollback %s: %w [exit=%d]", runID, err, rollbackExitCode(err))
	}

	// 5. Output the result.
	finalStatus := resp.GetChange().GetStatus()
	data := map[string]any{
		"run_id":            runID,
		"actor":             actor,
		"dry_run":           rollbackOptDryRun,
		"status":            finalStatus,
		"rolled_back_hosts": resp.GetRolledBackHosts(),
		"skipped_hosts":     resp.GetSkippedHosts(),
		"rollback_run_id":   resp.GetRollbackRunId(),
		"message":           resp.GetMessage(),
	}

	if optJSON {
		if err := PrintJSON(os.Stdout, map[string]any{
			"data":  data,
			"meta":  nil,
			"error": nil,
		}); err != nil {
			return err
		}
	} else if optQuiet {
		fmt.Fprintln(os.Stdout, runID)
	} else {
		printRollbackHuman(resp, actor, finalStatus)
	}

	// An incomplete rollback is an OUTCOME, not a crash: the service reports
	// it as Success=false with a status that says what happened. Exiting 0
	// there would tell the operator the change was rolled back when it was
	// not.
	if !resp.GetSuccess() {
		return fmt.Errorf("run %s finished in %q state [exit=1]", runID, finalStatus)
	}
	return nil
}

// printRollbackHuman renders the outcome of a rollback (or of a preview) for a
// terminal.
func printRollbackHuman(resp *pb.RollbackResponse, actor, finalStatus string) {
	if resp.GetChange() != nil && resp.GetChange().GetId() != "" {
		fmt.Fprintf(os.Stdout, "Run %s rollback by %s\n", resp.GetChange().GetId(), actor)
	} else {
		fmt.Fprintf(os.Stdout, "Rollback by %s\n", actor)
	}

	compensated := resp.GetRolledBackHosts()
	skipped := resp.GetSkippedHosts()
	label := "Compensated hosts"
	if resp.GetRollbackRunId() == "" && len(compensated) == 0 && len(skipped) == 0 {
		// A preview mints no rollback run and changes nothing; say so with the
		// same words the service used, so the two surfaces cannot disagree.
		label = "Would compensate hosts"
	}
	if len(compensated) > 0 {
		fmt.Fprintf(os.Stdout, "  %s: %s\n", label, strings.Join(compensated, ", "))
	} else {
		fmt.Fprintf(os.Stdout, "  %s: (none)\n", label)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stdout, "  Skipped hosts: %s\n", strings.Join(skipped, ", "))
	}
	if resp.GetRollbackRunId() != "" {
		fmt.Fprintf(os.Stdout, "  Rollback run: %s\n", resp.GetRollbackRunId())
	}
	if finalStatus != "" {
		fmt.Fprintf(os.Stdout, "  Status: %s\n", finalStatus)
	}
	if msg := resp.GetMessage(); msg != "" {
		fmt.Fprintf(os.Stdout, "  %s\n", msg)
	}
}

// rollbackExitCode maps a service refusal to the CLI's exit-code contract:
// 4 for a state/gate refusal (the run is in a status the rollback does not
// admit), 3 for a permission refusal, 1 for everything else.
func rollbackExitCode(err error) int {
	switch grpcstatus.Code(err) {
	case codes.FailedPrecondition:
		return 4
	case codes.PermissionDenied:
		return 3
	default:
		return 1
	}
}
