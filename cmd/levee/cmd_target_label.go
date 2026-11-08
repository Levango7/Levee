package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nexus/levee/internal/inventory"

	"github.com/spf13/cobra"
)

// targetLabelOptPort overrides the port a `target label` edit addresses. Zero
// means "whatever the address says", the same precedence `target import` gives
// a declaration's port field.
var targetLabelOptPort int

// newTargetLabelCmd builds the `levee target label` command group.
//
// Why an edit verb at all: labels are the one inventory fact an operator
// changes on its own (a host moves data centre, a role changes), and until now
// the only way to change one was to restate the host's whole label set in a
// YAML file and re-import it. That is not a safe ask: import REPLACES a row's
// labels, so a file that forgot a key silently dropped it.
func newTargetLabelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "label",
		Short: "Edit the labels of one inventory target",
		Long: "Set or remove labels on an existing target without restating the " +
			"rest of its label set. The `env` label is not editable here: it is " +
			"governed by target-ownership.yaml and changes through `target import`.",
	}
	cmd.PersistentFlags().IntVar(&targetLabelOptPort, "port", 0,
		"Port of the target (default: the port in <address>, else 22)")
	cmd.AddCommand(newTargetLabelSetCmd())
	cmd.AddCommand(newTargetLabelUnsetCmd())
	return cmd
}

// newTargetLabelSetCmd builds `levee target label set <address> key=value ...`.
func newTargetLabelSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <address> key=value [key=value ...]",
		Short: "Set labels on one target, keeping the labels it already has",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			set, err := parseLabelAssignments(args[1:])
			if err != nil {
				return err
			}
			return runTargetLabel(args[0], set, nil)
		},
	}
}

// newTargetLabelUnsetCmd builds `levee target label unset <address> key ...`.
func newTargetLabelUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <address> key [key ...]",
		Short: "Remove labels from one target",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys := make([]string, 0, len(args)-1)
			for _, a := range args[1:] {
				if strings.Contains(a, "=") {
					return fmt.Errorf("unset takes label keys, not assignments: %q (use `label set` to assign) [exit=2]", a)
				}
				keys = append(keys, a)
			}
			return runTargetLabel(args[0], nil, keys)
		},
	}
}

// parseLabelAssignments turns `key=value` arguments into a map. The value may be
// empty; only a missing key is refused, because a key with no name can never be
// matched by a filter or read back with confidence.
func parseLabelAssignments(args []string) (map[string]string, error) {
	set := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return nil, fmt.Errorf("expected key=value, got %q [exit=2]", a)
		}
		k = strings.TrimSpace(k)
		if k == "" {
			return nil, fmt.Errorf("label key must not be empty in %q [exit=2]", a)
		}
		set[k] = v
	}
	return set, nil
}

// runTargetLabel applies the edit and prints the row afterwards, so the operator
// sees the label set that landed rather than the one they asked for.
func runTargetLabel(address string, set map[string]string, unset []string) error {
	host, port, err := inventory.ParseAddress(address, targetLabelOptPort)
	if err != nil {
		return fmt.Errorf("%w [exit=2]", err)
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	row, changed, err := inventory.SetTargetLabels(ctx, store, host, port, set, unset)
	if err != nil {
		return err
	}

	output := map[string]any{
		"target":  row.Hostname,
		"port":    row.Port,
		"labels":  row.Labels,
		"changed": changed,
	}
	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{"data": output, "meta": nil, "error": nil})
	}

	labels := inventory.FormatLabels(row.Labels)
	if labels == "" {
		labels = "(none)"
	}
	if !changed {
		fmt.Fprintf(os.Stdout, "%s:%d unchanged (labels: %s)\n", row.Hostname, row.Port, labels)
		return nil
	}
	fmt.Fprintf(os.Stdout, "%s:%d labels: %s\n", row.Hostname, row.Port, labels)
	return nil
}
