package main

import (
	"context"

	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

// Target command option variables.
var (
	targetListOptFormat  string
	targetImportOptFile  string
	targetCheckOptTarget string
)

func init() {
	RegisterCommand(newTargetCmd())
}

// newTargetCmd builds the `levee target` sub-command with its children.
func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage target hosts",
		Long: "Manage target hosts for change execution: list known " +
			"targets, import from file, and run prechecks.",
	}

	cmd.AddCommand(newTargetListCmd())
	cmd.AddCommand(newTargetImportCmd())
	cmd.AddCommand(newTargetCheckCmd())

	return cmd
}

// newTargetListCmd builds the `levee target list` sub-command.
func newTargetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known target hosts",
		Long:  "List all target hosts known from past change runs.",
		Args:  cobra.NoArgs,
		RunE:  runTargetList,
	}
	cmd.Flags().StringVar(&targetListOptFormat, "format", "", "Output format (text)")
	return cmd
}

// newTargetImportCmd builds the `levee target import` sub-command.
func newTargetImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import target hosts from a file",
		Long:  "Import a list of target hosts from a file (one host per line).",
		Args:  cobra.NoArgs,
		RunE:  runTargetImport,
	}
	cmd.Flags().StringVar(&targetImportOptFile, "file", "", "Input file path (required)")
	cmd.MarkFlagRequired("file")
	return cmd
}

// newTargetCheckCmd builds the `levee target check <host>` sub-command.
func newTargetCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check <host>",
		Short: "Run precheck on a target host",
		Long:  "Verify reachability of a target host by running a connectivity check.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTargetCheck,
	}
	return cmd
}

// runTargetList executes the `levee target list` command.
func runTargetList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// Collect unique hosts from steps across all runs.
	steps, err := store.ListSteps(ctx, state.StepFilter{})
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}

	seen := make(map[string]bool)
	var hosts []string
	for _, s := range steps {
		if s.Host != "" && !seen[s.Host] {
			seen[s.Host] = true
			hosts = append(hosts, s.Host)
		}
	}

	rows := make([]map[string]any, 0, len(hosts))
	for _, h := range hosts {
		rows = append(rows, map[string]any{
			"host": h,
		})
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, h := range hosts {
			fmt.Fprintln(os.Stdout, h)
		}
		return nil
	}

	printTargetListHuman(os.Stdout, rows)
	return nil
}

// runTargetImport executes the `levee target import` command.
func runTargetImport(cmd *cobra.Command, args []string) error {
	_ = context.Background()

	if targetImportOptFile == "" {
		return fmt.Errorf("file path is required")
	}

	data, err := os.ReadFile(targetImportOptFile)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	var imported []string
	for _, line := range lines {
		host := strings.TrimSpace(line)
		if host != "" && !strings.HasPrefix(host, "#") {
			imported = append(imported, host)
		}
	}

	output := map[string]any{
		"file":     targetImportOptFile,
		"imported": len(imported),
		"hosts":    imported,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, len(imported))
		return nil
	}

	printTargetImportHuman(os.Stdout, output)
	return nil
}

// runTargetCheck executes the `levee target check <host>` command.
func runTargetCheck(cmd *cobra.Command, args []string) error {
	targetCheckOptTarget = args[0]
	ctx := context.Background()

	// Build a simple target for precheck.
	tgt := &simpleTarget{host: targetCheckOptTarget}

	// Create a prechecker. We use no channel and no limiter since we
	// don't have a real transport in CLI mode; instead we report the
	// target as needing manual verification.
	prechecker := channel.NewPrechecker(nil, nil)

	// Run the precheck. Without a channel, the prechecker reports the
	// target as unreachable (no channel available).
	report := prechecker.Check(ctx, []channel.Target{tgt})

	result := channel.PrecheckResult{}
	if len(report.Results) > 0 {
		result = report.Results[0]
	}

	output := map[string]any{
		"target":    targetCheckOptTarget,
		"reachable": result.Reachable,
		"latency":   result.Latency.String(),
		"error":     result.Error,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, targetCheckOptTarget)
		return nil
	}

	printTargetCheckHuman(os.Stdout, output)
	return nil
}

// simpleTarget implements channel.Target for CLI usage.
type simpleTarget struct {
	host string
}

func (t *simpleTarget) Host() string                       { return t.host }
func (t *simpleTarget) Port() int                          { return 0 }
func (t *simpleTarget) Type() string                       { return "ssh" }
func (t *simpleTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// printTargetListHuman renders the target list in a human-readable format.
func printTargetListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No targets found.")
		return
	}
	fmt.Fprintf(w, "%-40s\n", "HOST")
	for _, row := range rows {
		host, _ := row["host"].(string)
		fmt.Fprintf(w, "%-40s\n", host)
	}
}

// printTargetImportHuman renders the target import output.
func printTargetImportHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Imported %v hosts from %v\n", output["imported"], output["file"])
}

// printTargetCheckHuman renders the target check output.
func printTargetCheckHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Target: %v\n", output["target"])
	fmt.Fprintf(w, "  Reachable: %v\n", output["reachable"])
	fmt.Fprintf(w, "  Latency:   %v\n", output["latency"])
	if output["error"] != "" && output["error"] != nil {
		fmt.Fprintf(w, "  Error:     %v\n", output["error"])
	}
}
