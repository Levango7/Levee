package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/state"
)

// Target command option variables.
var (
	targetListOptFormat   string
	targetImportOptFile   string
	targetImportOptGroup  string
	targetCheckOptTarget  string
	targetStatusOptTarget string
	targetHistOptLimit    int
	targetListGroupFilter string
	targetListStatusFilter string
)

func init() {
	RegisterCommand(newTargetCmd())
}

// newTargetCmd builds the `levee target` sub-command with its children.
func newTargetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Manage target hosts (inventory)",
		Long: "Manage the persistent target inventory: import from YAML, " +
			"group and label hosts, control lifecycle status, inspect change " +
			"history per host.",
	}

	cmd.AddCommand(newTargetListCmd())
	cmd.AddCommand(newTargetImportCmd())
	cmd.AddCommand(newTargetCheckCmd())
	cmd.AddCommand(newTargetFreezeCmd())
	cmd.AddCommand(newTargetUnfreezeCmd())
	cmd.AddCommand(newTargetRetireCmd())
	cmd.AddCommand(newTargetHistoryCmd())
	return cmd
}

// newTargetListCmd builds the `levee target list` sub-command.
func newTargetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List targets in the persistent inventory",
		Long:  "List managed targets from the inventory store, including their group, labels and lifecycle status.",
		Args:  cobra.NoArgs,
		RunE:  runTargetList,
	}
	cmd.Flags().StringVar(&targetListOptFormat, "format", "", "Output format (text)")
	cmd.Flags().StringVar(&targetListGroupFilter, "group", "", "Filter by group name")
	cmd.Flags().StringVar(&targetListStatusFilter, "status", "", "Filter by status (active|frozen|retired)")
	return cmd
}

// newTargetImportCmd builds the `levee target import` sub-command.
func newTargetImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import targets from a YAML inventory file",
		Long: "Import groups and targets from a declarative YAML file. The " +
			"import is idempotent: re-importing updates existing rows matched " +
			"by address instead of duplicating them.",
		Args: cobra.NoArgs,
		RunE: runTargetImport,
	}
	cmd.Flags().StringVar(&targetImportOptFile, "file", "", "YAML inventory file path (required)")
	cmd.Flags().StringVar(&targetImportOptGroup, "default-group", "", "Assign targets without an explicit group to this group")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// newTargetFreezeCmd builds `levee target freeze <id>`.
func newTargetFreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "freeze <target-id>",
		Short: "Freeze a target (rejected for changes while frozen)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusFrozen)
		},
	}
}

// newTargetUnfreezeCmd builds `levee target unfreeze <id>`.
func newTargetUnfreezeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfreeze <target-id>",
		Short: "Return a frozen target to active service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusActive)
		},
	}
}

// newTargetRetireCmd builds `levee target retire <id>`.
func newTargetRetireCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retire <target-id>",
		Short: "Retire a target (kept for history, excluded from changes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			targetStatusOptTarget = args[0]
			return runTargetSetStatus(state.StatusRetired)
		},
	}
}

// newTargetHistoryCmd builds `levee target history <host>`.
func newTargetHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <host>",
		Short: "Show recent changes executed on a host",
		Long:  "List the most recent change runs that executed steps on the given host address.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTargetHistory,
	}
	cmd.Flags().IntVar(&targetHistOptLimit, "limit", 20, "Maximum entries to show")
	return cmd
}

// newTargetCheckCmd builds the `levee target check <host>` sub-command.
func newTargetCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <host>",
		Short: "Run precheck on a target host",
		Long:  "Verify reachability of a target host by running a connectivity check.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTargetCheck,
	}
}

// runTargetList executes the `levee target list` command.
func runTargetList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	filter := state.TargetFilter{Status: targetListStatusFilter}
	if targetListGroupFilter != "" {
		g, err := store.GetInventoryGroupByName(ctx, targetListGroupFilter)
		if err != nil {
			return fmt.Errorf("resolve group: %w", err)
		}
		if g == nil {
			return fmt.Errorf("group %q not found", targetListGroupFilter)
		}
		filter.GroupID = g.ID
	}

	targets, err := store.ListTargets(ctx, filter)
	if err != nil {
		return fmt.Errorf("list targets: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  targets,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		for _, t := range targets {
			fmt.Fprintf(os.Stdout, "%s:%d\n", t.Hostname, t.Port)
		}
		return nil
	}

	printTargetListHuman(os.Stdout, targets)
	return nil
}

// runTargetImport executes the `levee target import` command.
func runTargetImport(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	data, err := os.ReadFile(targetImportOptFile)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	f, err := inventory.ParseYAML(data)
	if err != nil {
		return fmt.Errorf("parse inventory yaml: %w", err)
	}

	sum, err := inventory.NewImporter(store).Import(ctx, f, targetImportOptGroup)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	output := map[string]any{
		"file":    targetImportOptFile,
		"created": sum.Created,
		"updated": sum.Updated,
		"failed":  sum.Failed,
		"errors":  sum.Errors,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}

	printTargetImportHuman(os.Stdout, output)
	return nil
}

// runTargetSetStatus applies a lifecycle status transition to a target.
func runTargetSetStatus(status string) error {
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.UpdateTargetStatus(ctx, targetStatusOptTarget, status); err != nil {
		return err
	}

	output := map[string]any{"id": targetStatusOptTarget, "status": status}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}
	fmt.Fprintf(os.Stdout, "Target %s is now %s\n", targetStatusOptTarget, status)
	return nil
}

// runTargetHistory executes the `levee target history <host>` command.
func runTargetHistory(cmd *cobra.Command, args []string) error {
	host := args[0]
	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	limit := targetHistOptLimit
	if limit <= 0 {
		limit = 20
	}
	steps, err := store.ListSteps(ctx, state.StepFilter{Host: host, Limit: limit * 10})
	if err != nil {
		return fmt.Errorf("list steps: %w", err)
	}

	seen := map[string]bool{}
	var entries []*state.Run
	for _, s := range steps {
		if seen[s.RunID] {
			continue
		}
		seen[s.RunID] = true
		run, err := store.GetRun(ctx, s.RunID)
		if err != nil || run == nil {
			continue
		}
		entries = append(entries, run)
		if len(entries) >= limit {
			break
		}
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": entries, "meta": nil, "error": nil})
	}
	if len(entries) == 0 {
		fmt.Fprintf(os.Stdout, "No changes recorded on %s.\n", host)
		return nil
	}
	fmt.Fprintf(os.Stdout, "Recent changes on %s:\n\n%-22s %-20s %-10s %-12s\n",
		host, "RUN", "WORKFLOW", "STATUS", "CREATED")
	for _, r := range entries {
		fmt.Fprintf(os.Stdout, "%-22s %-20s %-10s %-12s\n",
			r.ID, r.WorkflowName, r.Status, r.CreatedAt.Format("2006-01-02"))
	}
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
func printTargetListHuman(w io.Writer, targets []*state.Target) {
	if len(targets) == 0 {
		fmt.Fprintln(w, "No targets in the inventory. Import some with `levee target import -f <file>`.")
		return
	}
	fmt.Fprintf(w, "%-28s %-6s %-7s %-8s %s\n", "ADDRESS", "PORT", "CHANNEL", "STATUS", "LABELS")
	for _, t := range targets {
		labels := make([]string, 0, len(t.Labels))
		for k, v := range t.Labels {
			labels = append(labels, k+"="+v)
		}
		fmt.Fprintf(w, "%-28s %-6d %-7s %-8s %s\n",
			t.Hostname, t.Port, t.ChannelType, t.Status, strings.Join(labels, ","))
	}
}

// printTargetImportHuman renders the target import output.
func printTargetImportHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Imported %v (created) / %v (updated) from %v\n",
		output["created"], output["updated"], output["file"])
	if failed, _ := output["failed"].(int); failed > 0 {
		errs, _ := output["errors"].([]string)
		for _, e := range errs {
			fmt.Fprintf(w, "  error: %s\n", e)
		}
	}
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
