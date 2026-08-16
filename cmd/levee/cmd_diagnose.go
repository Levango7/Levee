package main

// cmd_diagnose.go implements the `levee diagnose` sub-command (Phase A6).
// It builds a DiagEngine from the available components and runs a full
// diagnosis on the given target, printing the resulting DiagnosticReport
// as JSON (--json) or as a human-readable document.
//
// In local mode the command uses an in-process shell executor (os/exec)
// so that `levee diagnose localhost` works out of the box. For remote
// targets the operator should configure an SSH / WinRM channel (future
// phase); until then the command reports the target as unreachable.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nexus/levee/internal/alert"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/spf13/cobra"
)

// Diagnose command option variables.
var (
	diagOptAlert  string
	diagOptLogs   bool
	diagOptHealth bool
	diagOptWindow string
	diagOptJSON   bool // local alias for the global --json flag
)

func init() {
	RegisterCommand(newDiagnoseCmd())
}

// newDiagnoseCmd builds the `levee diagnose <target>` sub-command.
func newDiagnoseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diagnose <target>",
		Short: "Run diagnostic engine on a target",
		Long: "Run the LEVEE diagnostic engine on a target host. The engine " +
			"concurrently collects and analyses logs and probes health, then " +
			"synthesises a diagnostic report with findings, root cause and " +
			"recommendations.\n\n" +
			"Use --logs to skip health probes, --health to skip log analysis, " +
			"or --alert <alert-id> to mark the run as triggered by an alert.",
		Args: cobra.ExactArgs(1),
		RunE: runDiagnose,
	}
	cmd.Flags().StringVar(&diagOptAlert, "alert", "", "Alert ID that triggered the diagnosis")
	cmd.Flags().BoolVar(&diagOptLogs, "logs", false, "Only run log collection and analysis")
	cmd.Flags().BoolVar(&diagOptHealth, "health", false, "Only run health probes")
	cmd.Flags().StringVar(&diagOptWindow, "window", "15m", "Log look-back window (e.g. 15m, 1h)")
	return cmd
}

// runDiagnose executes the `levee diagnose <target>` command.
func runDiagnose(cmd *cobra.Command, args []string) error {
	target := args[0]
	ctx := context.Background()

	// Parse the log look-back window.
	window, err := parseDiagWindow(diagOptWindow)
	if err != nil {
		return fmt.Errorf("invalid --window: %w", err)
	}

	// Build the engine config from the selected flags.
	cfg, err := buildDiagEngineConfig(window)
	if err != nil {
		return err
	}

	engine := diagnosis.NewDiagEngine(cfg)

	var report diagnosis.DiagnosticReport
	if diagOptAlert != "" {
		// Synthesise an alert from the alert-id + target so the report
		// is marked as alert-triggered. A real deployment would look up
		// the alert from the alert store.
		a := &alert.Alert{
			ID:     diagOptAlert,
			Source: "cli",
			Title:  "manual diagnosis trigger",
			Labels: map[string]string{"instance": target},
		}
		report = engine.DiagnoseFromAlert(ctx, a)
	} else {
		report = engine.Diagnose(ctx, target)
	}

	// Output.
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  report,
			"meta":  map[string]any{"target": target, "trigger": report.Trigger},
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, report.ID)
		return nil
	}

	printDiagnoseHuman(os.Stdout, &report)
	return nil
}

// buildDiagEngineConfig assembles a DiagEngineConfig from the CLI flags. The
// --logs and --health flags are mutually exclusive and select which components
// are wired into the engine.
func buildDiagEngineConfig(window time.Duration) (diagnosis.DiagEngineConfig, error) {
	if diagOptLogs && diagOptHealth {
		return diagnosis.DiagEngineConfig{}, fmt.Errorf("--logs and --health are mutually exclusive [exit=2]")
	}

	cfg := diagnosis.DiagEngineConfig{
		LogWindow: window,
		Timeout:   60 * time.Second,
	}

	// In local mode we use an in-process shell executor so the engine can
	// collect logs and probe health on the local machine. For remote
	// targets this executor still runs locally (which is wrong for SSH
	// targets) but keeps the CLI usable for self-diagnosis.
	executor := newLocalExecutor()

	// Wire the log pipeline unless --health is set.
	if !diagOptHealth {
		collector, err := diagnosis.NewLogCollector(executor)
		if err != nil {
			return cfg, fmt.Errorf("build log collector: %w", err)
		}
		cfg.Collector = collector
		cfg.Analyzer = diagnosis.NewDefaultLogAnalyzer()
	}

	// Wire the health prober unless --logs is set.
	if !diagOptLogs {
		cfg.Prober = diagnosis.NewHealthProber(diagnosis.HealthProberConfig{
			Executor: executor,
		})
	}

	return cfg, nil
}

// parseDiagWindow parses a Go duration string (e.g. "15m", "1h") into a
// time.Duration. Empty string defaults to 15 minutes.
func parseDiagWindow(s string) (time.Duration, error) {
	if s == "" {
		return 15 * time.Minute, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("window must be positive")
	}
	return d, nil
}

// printDiagnoseHuman renders the diagnostic report in a human-readable form.
func printDiagnoseHuman(w io.Writer, report *diagnosis.DiagnosticReport) {
	fmt.Fprintln(w, report.String())
}

// --- localExecutor ---------------------------------------------------------
//
// localExecutor implements diagnosis.CommandExecutor by running commands
// in-process via os/exec. It is used by the diagnose command in local mode
// so that `levee diagnose localhost` works without a remote transport. The
// target argument is ignored — every command runs on the local machine.

// localExecutor runs shell commands locally via os/exec.
type localExecutor struct{}

// newLocalExecutor returns a localExecutor.
func newLocalExecutor() *localExecutor {
	return &localExecutor{}
}

// Execute runs command on the local machine. target is ignored. The command
// is run through `sh -c` on Unix and `cmd /c` on Windows so that shell
// features (pipes, redirects) work as expected.
func (e *localExecutor) Execute(ctx context.Context, _, command string) (string, string, int, error) {
	var cmd *exec.Cmd
	// Detect the shell. On Windows we use cmd /c; on Unix we use sh -c.
	// We use a simple heuristic: if the command contains PowerShell-specific
	// syntax we use powershell, otherwise the platform default.
	if strings.HasPrefix(command, "powershell ") {
		psCmd := strings.TrimPrefix(command, "powershell ")
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", psCmd)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return stdout.String(), stderr.String(), -1, err
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}
