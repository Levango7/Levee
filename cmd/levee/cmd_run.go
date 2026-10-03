package main

// cmd_run.go — `levee run --shell <command>` (MVP task T058).
//
// This is the LOCAL DEBUG ESCAPE HATCH, not a governed execution path: it
// runs one command line on the machine the CLI runs on, through the platform
// shell, with a timeout and captured output. It deliberately does NOT go
// through plan → approval → batch → execute, and it writes nothing to the
// audit / trace chains — the help text says exactly that, so nobody mistakes
// a `run` invocation for a governed change. The ShellRunner it wraps has
// existed since the MVP (internal/executor/shell_run.go) with zero callers;
// registering it as an explicitly-labelled local debug command is the
// disposition the 2026-08-15 review asked for (P2-07).
//
// Exit-code contract: the child's exit code propagates as the CLI's exit
// code via the established "[exit=N]" marker (root.go's exitCodeFor); a
// timeout maps to 8, the same code the global --timeout convention uses.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/executor"
)

var (
	runOptShell          string
	runOptCommandTimeout time.Duration
)

// resetRunFlags restores the defaults; called from tests so repeated
// invocations do not leak flag state (same pattern as resetCompileFlags).
func resetRunFlags() {
	runOptShell = ""
	runOptCommandTimeout = executor.DefaultShellTimeout
}

func init() {
	RegisterCommand(newRunCmd())
}

// newRunCmd builds the `levee run --shell <command>` sub-command.
func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run --shell <command>",
		Short: "Run one shell command locally (debug escape hatch, NOT governed)",
		Long: "Run one command line on the LOCAL machine and print its result.\n\n" +
			"This is a debug escape hatch, not a change execution path: the command\n" +
			"does NOT go through plan / approval / batches and writes nothing to the\n" +
			"audit or trace chains. Use the change pipeline (plan → approve → apply)\n" +
			"for anything that needs governance.\n\n" +
			"Output: command, exit code, duration, stdout and stderr. With --json the\n" +
			"result is a structured document; on a non-zero child exit the process\n" +
			"exit code is the child's exit code (timeout -> 8).",
		Args: cobra.NoArgs,
		RunE: runRun,
	}
	cmd.Flags().StringVar(&runOptShell, "shell", "",
		"command line to run locally (required; runs through the platform shell)")
	cmd.Flags().DurationVar(&runOptCommandTimeout, "command-timeout", executor.DefaultShellTimeout,
		"wall-clock budget for the child command (distinct from the global --timeout)")
	return cmd
}

func runRun(cmd *cobra.Command, args []string) error {
	if strings.TrimSpace(runOptShell) == "" {
		return fmt.Errorf("run: --shell <command> is required — this command runs one local command line by design; it is not a workflow runner [exit=2]")
	}

	// cmd.Context() is nil when the command is invoked directly (tests);
	// fall back to Background so the runner always gets a real parent.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	runner := executor.NewShellRunner().WithTimeout(runOptCommandTimeout)
	result, err := runner.Run(ctx, runOptShell)
	if result == nil {
		// Spawn failure before anything ran.
		return fmt.Errorf("run: %w [exit=1]", err)
	}

	// Timeout: map to the global timeout exit code (8), and still show
	// whatever was captured before the kill.
	if result.Err != nil && result.ExitCode == -1 {
		if optJSON {
			return fmt.Errorf("run: command timed out after %s (%d bytes of stdout captured) [exit=8]", result.Duration, len(result.Stdout))
		}
		printRunResult(os.Stdout, result)
		return fmt.Errorf("run: command timed out after %s [exit=8]", result.Duration)
	}

	// Child ran to completion (any exit code). Human mode prints the full
	// result (stdout payload included) and maps a non-zero child exit to
	// the CLI exit code; JSON mode prints the result document on success
	// and, on failure, returns the standard error envelope carrying a
	// compact summary — every other command has the same failure shape.
	if !optJSON || result.ExitCode == 0 {
		printRunResult(os.Stdout, result)
	}
	if result.ExitCode != 0 {
		if optJSON {
			return fmt.Errorf("run: command exited with code %d after %s; stderr: %s [exit=%d]",
				result.ExitCode, result.Duration, lastLines(result.Stderr, 3), result.ExitCode)
		}
		return fmt.Errorf("run: command exited with code %d [exit=%d]", result.ExitCode, result.ExitCode)
	}
	return nil
}

// printRunResult renders the result in the mode selected by the global
// --json flag.
func printRunResult(w io.Writer, r *executor.ShellRunResult) {
	if optJSON {
		_ = PrintJSON(w, map[string]any{
			"command":   r.Command,
			"exit_code": r.ExitCode,
			"stdout":    r.Stdout,
			"stderr":    r.Stderr,
			"duration":  r.Duration.String(),
		})
		return
	}
	fmt.Fprintf(w, "command:  %s\n", r.Command)
	fmt.Fprintf(w, "exit:     %d\n", r.ExitCode)
	fmt.Fprintf(w, "duration: %s\n", r.Duration)
	if r.Stdout != "" {
		fmt.Fprintf(w, "--- stdout ---\n%s", r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			fmt.Fprintln(w)
		}
	}
	if r.Stderr != "" {
		fmt.Fprintf(w, "--- stderr ---\n%s", r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			fmt.Fprintln(w)
		}
	}
}

// lastLines returns at most n trailing lines of s, for error messages that
// must stay one line.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	joined := strings.Join(lines, " | ")
	if len(joined) > 400 {
		joined = joined[len(joined)-400:]
	}
	if joined == "" {
		joined = "(none)"
	}
	return joined
}
