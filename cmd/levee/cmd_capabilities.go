package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/targetcap"
	"github.com/nexus/levee/internal/wiring"
)

var capabilitiesOptTarget string

func init() {
	RegisterCommand(newCapabilitiesCmd())
}

func newCapabilitiesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "capabilities [host]",
		Short: "Probe which executor actions a target actually supports",
		Long: "Run read-only capability probes through the same inventory, credential " +
			"resolver and channel used by execution. Output is levee.capabilities/v1 " +
			"with supported/unsupported/unknown and a reason for every action. " +
			"Observation is not an acceptance verdict.",
		Args: cobra.MaximumNArgs(1),
		RunE: runCapabilities,
	}
	cmd.Flags().StringVar(&capabilitiesOptTarget, "target", "", "Inventory host to probe (or pass it as the positional argument)")
	return cmd
}

func runCapabilities(cmd *cobra.Command, args []string) error {
	host := capabilitiesOptTarget
	if host == "" && len(args) == 1 {
		host = strings.TrimSpace(args[0])
	}
	if host == "" {
		return fmt.Errorf("target is required: levee capabilities --target <host>")
	}
	mode, err := resolveServiceMode()
	if err != nil {
		return err
	}
	if mode != modeLocal {
		return fmt.Errorf("capabilities probes run against the local inventory and channels; remote mode is not supported")
	}
	store, err := openStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	engine := wiring.NewEngine(store, wiring.WithCredentialResolver(cliCredentialResolver(store)))
	return runCapabilitiesWithEngine(cmd.Context(), engine, host, os.Stdout)
}

type capabilityDialer interface {
	Dial(ctx context.Context, host string) (channel.Channel, error)
}

func runCapabilitiesWithEngine(ctx context.Context, engine capabilityDialer, host string, writer io.Writer) error {
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("target is required")
	}
	ch, err := engine.Dial(ctx, host)
	if err != nil {
		return fmt.Errorf("dial target %q: %w", host, err)
	}
	defer func() { _ = ch.Close() }()
	report, err := targetcap.Probe(ctx, ch, host)
	if err != nil {
		return fmt.Errorf("probe target %q: %w", host, err)
	}
	if optJSON {
		return PrintJSON(writer, map[string]any{"data": report, "meta": nil, "error": nil})
	}
	if optQuiet {
		fmt.Fprintf(writer, "%d/%d supported; %d unknown\n", report.Summary.Supported, len(report.Actions), report.Summary.Unknown)
		return nil
	}
	renderCapabilitiesHuman(writer, report)
	return nil
}

func renderCapabilitiesHuman(writer io.Writer, report *targetcap.Report) {
	fmt.Fprintf(writer, "Target: %s (%s)\n", report.Target, report.SchemaVersion)
	fmt.Fprintf(writer, "Evidence: %s\n", report.Evidence)
	if report.Kernel != "" {
		fmt.Fprintf(writer, "Kernel: %s\n", report.Kernel)
	}
	if id := report.OS["ID"]; id != "" {
		fmt.Fprintf(writer, "OS: %s %s\n", id, report.OS["VERSION_ID"])
	}
	fmt.Fprintf(writer, "Package manager: %s\n", orUnknown(report.PackageManager))
	fmt.Fprintf(writer, "Init system: %s\n", orUnknown(report.InitSystem))
	for _, tool := range []string{"psql", "pg_dump", "mysql", "pt-online-schema-change"} {
		if version := report.Tools[tool]; version != "" {
			fmt.Fprintf(writer, "%s: %s\n", tool, version)
		}
	}
	fmt.Fprintln(writer, "ACTION                         STATUS       REASON")
	for _, action := range report.Actions {
		fmt.Fprintf(writer, "%-30s %-12s %s\n", action.Name, action.Status, action.Reason)
	}
	fmt.Fprintf(writer, "Summary: %d supported, %d unsupported, %d unknown\n",
		report.Summary.Supported, report.Summary.Unsupported, report.Summary.Unknown)
}

func orUnknown(value string) string {
	if value == "" {
		return "not detected"
	}
	return value
}
