package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nexus/levee/internal/pause"
	"github.com/spf13/cobra"
)

// pauseAllOptReason holds the value of the --reason flag for pause-all.
var pauseAllOptReason string

// resumeAllOptReason holds the value of the --reason flag for resume-all.
var resumeAllOptReason string

func init() {
	RegisterCommand(newPauseCmd())
	RegisterCommand(newResumeCmd())
	RegisterCommand(newPauseAllCmd())
	RegisterCommand(newResumeAllCmd())
}

// newPauseCmd builds the `levee pause <run-id>` sub-command.
func newPauseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause <run-id>",
		Short: "Pause a single change run",
		Long: "Pause a single change run. The run must be in \"running\" or " +
			"\"pending\" state. Paused runs preserve all state and can be " +
			"resumed later with the resume command.",
		Args: cobra.ExactArgs(1),
		RunE: runPause,
	}
	return cmd
}

// newResumeCmd builds the `levee resume <run-id>` sub-command.
func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Resume a paused change run",
		Long: "Resume a paused change run. The run must be in \"paused\" " +
			"state. Upon resumption the run transitions back to \"running\" " +
			"and batch execution continues from where it left off.",
		Args: cobra.ExactArgs(1),
		RunE: runResume,
	}
	return cmd
}

// newPauseAllCmd builds the `levee pause-all` sub-command.
func newPauseAllCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause-all",
		Short: "Pause all change runs globally",
		Long: "Pause every running or pending change run. This is a high-privilege " +
			"operation that requires the \"pause:all\" permission. An audit " +
			"entry is recorded for each affected run and a global summary.",
		Args: cobra.NoArgs,
		RunE: runPauseAll,
	}
	cmd.Flags().StringVar(&pauseAllOptReason, "reason", "", "Reason for global pause (recorded in audit)")
	return cmd
}

// newResumeAllCmd builds the `levee resume-all` sub-command.
func newResumeAllCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume-all",
		Short: "Resume all paused change runs globally",
		Long: "Resume every paused change run. This is a high-privilege " +
			"operation that requires the \"resume:all\" permission. An audit " +
			"entry is recorded for each affected run and a global summary.",
		Args: cobra.NoArgs,
		RunE: runResumeAll,
	}
	cmd.Flags().StringVar(&resumeAllOptReason, "reason", "", "Reason for global resume (recorded in audit)")
	return cmd
}

// runPause executes the `levee pause <run-id>` command.
func runPause(cmd *cobra.Command, args []string) error {
	pauseOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Create the PauseManager and pause the run.
	mgr := pause.NewPauseManager(store)
	if err := mgr.PauseRun(ctx, pauseOptRunID, actor); err != nil {
		return mapPauseError(err)
	}

	// 3. Output the result.
	output := map[string]any{
		"run_id": pauseOptRunID,
		"action": "paused",
		"actor":  actor,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, pauseOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s paused by %s\n", pauseOptRunID, actor)
	return nil
}

// runResume executes the `levee resume <run-id>` command.
func runResume(cmd *cobra.Command, args []string) error {
	resumeOptRunID := args[0]
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Create the PauseManager and resume the run.
	mgr := pause.NewPauseManager(store)
	if err := mgr.ResumeRun(ctx, resumeOptRunID, actor); err != nil {
		return mapPauseError(err)
	}

	// 3. Output the result.
	output := map[string]any{
		"run_id": resumeOptRunID,
		"action": "resumed",
		"actor":  actor,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, resumeOptRunID)
		return nil
	}

	fmt.Fprintf(os.Stdout, "Run %s resumed by %s\n", resumeOptRunID, actor)
	return nil
}

// runPauseAll executes the `levee pause-all` command.
func runPauseAll(cmd *cobra.Command, args []string) error {
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Build the permission checker from environment.
	perm := newCLIPermissionChecker()

	// 3. Create the PauseManager and pause all runs.
	mgr := pause.NewPauseManager(store)
	result, err := mgr.PauseAll(ctx, actor, perm)
	if err != nil {
		return mapPauseError(err)
	}

	// 4. Output the result.
	output := map[string]any{
		"action":   "pause_all",
		"actor":    actor,
		"reason":   pauseAllOptReason,
		"affected": result.Affected,
		"failed":   formatFailedMap(result.Failed),
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, id := range result.Affected {
			fmt.Fprintln(os.Stdout, id)
		}
		return nil
	}

	fmt.Fprintf(os.Stdout, "Paused %d runs by %s\n", len(result.Affected), actor)
	if len(result.Failed) > 0 {
		fmt.Fprintf(os.Stdout, "Failed: %d\n", len(result.Failed))
	}
	return nil
}

// runResumeAll executes the `levee resume-all` command.
func runResumeAll(cmd *cobra.Command, args []string) error {
	actor := currentActor()

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Build the permission checker from environment.
	perm := newCLIPermissionChecker()

	// 3. Create the PauseManager and resume all runs.
	mgr := pause.NewPauseManager(store)
	result, err := mgr.ResumeAll(ctx, actor, perm)
	if err != nil {
		return mapPauseError(err)
	}

	// 4. Output the result.
	output := map[string]any{
		"action":   "resume_all",
		"actor":    actor,
		"reason":   resumeAllOptReason,
		"affected": result.Affected,
		"failed":   formatFailedMap(result.Failed),
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, id := range result.Affected {
			fmt.Fprintln(os.Stdout, id)
		}
		return nil
	}

	fmt.Fprintf(os.Stdout, "Resumed %d runs by %s\n", len(result.Affected), actor)
	if len(result.Failed) > 0 {
		fmt.Fprintf(os.Stdout, "Failed: %d\n", len(result.Failed))
	}
	return nil
}

// mapPauseError translates pause manager errors to CLI-friendly messages
// with appropriate exit codes.
func mapPauseError(err error) error {
	if errors.Is(err, pause.ErrRunNotFound) {
		return fmt.Errorf("run not found: %w [exit=1]", err)
	}
	if errors.Is(err, pause.ErrNotPausable) {
		return fmt.Errorf("cannot pause: %w [exit=1]", err)
	}
	if errors.Is(err, pause.ErrNotResumable) {
		return fmt.Errorf("cannot resume: %w [exit=1]", err)
	}
	if errors.Is(err, pause.ErrPermissionDenied) {
		return fmt.Errorf("permission denied: %w [exit=3]", err)
	}
	if errors.Is(err, pause.ErrEmptyRunID) || errors.Is(err, pause.ErrEmptyActor) {
		return fmt.Errorf("invalid input: %w [exit=2]", err)
	}
	return fmt.Errorf("pause: %w", err)
}

// newCLIPermissionChecker builds a PermissionChecker from the CLI environment.
// It reads the LEVEE_PERMISSIONS environment variable which should contain a
// comma-separated list of permissions granted to the current actor. When the
// variable is not set, the actor is granted both pause:all and resume:all
// (admin-by-default for CLI mode).
func newCLIPermissionChecker() pause.PermissionChecker {
	permsStr := os.Getenv("LEVEE_PERMISSIONS")
	if permsStr == "" {
		// Admin-by-default in CLI mode: grant all pause/resume permissions.
		return pause.NewSimplePermissionChecker(map[string][]string{
			currentActor(): {pause.PermissionPauseAll, pause.PermissionResumeAll},
		})
	}
	perms := strings.Split(permsStr, ",")
	return pause.NewSimplePermissionChecker(map[string][]string{
		currentActor(): perms,
	})
}

// formatFailedMap converts a map[string]error to a map[string]string for
// JSON serialization.
func formatFailedMap(failed map[string]error) map[string]string {
	if len(failed) == 0 {
		return nil
	}
	result := make(map[string]string, len(failed))
	for k, v := range failed {
		result[k] = v.Error()
	}
	return result
}
