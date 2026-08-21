package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// linkOptIncident holds the value of the --incident flag for the link command.
var linkOptIncident string

func init() {
	RegisterCommand(newLinkCmd())
}

// newLinkCmd builds the `levee link` sub-command.
func newLinkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "link <run-id> --incident <incident-id>",
		Short: "Associate a change run with an incident",
		Long: "Link a change run to an incident by updating the run's " +
			"metadata. This association is used for traceability between " +
			"change executions and incident records.",
		Args: cobra.ExactArgs(1),
		RunE: runLink,
	}
	cmd.Flags().StringVar(&linkOptIncident, "incident", "", "Incident ID to associate with the run (required)")
	_ = cmd.MarkFlagRequired("incident")
	return cmd
}

// runLink executes the `levee link <run-id> --incident <id>` command.
func runLink(cmd *cobra.Command, args []string) error {
	linkOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// 2. Get the run.
	run, err := store.GetRun(ctx, linkOptRunID)
	if err != nil {
		return fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return fmt.Errorf("run %q not found", linkOptRunID)
	}

	// 3. Update the run's incident association.
	run.IncidentID = linkOptIncident
	if err := store.UpdateRun(ctx, run); err != nil {
		return fmt.Errorf("update run: %w", err)
	}

	// 4. Output the result.
	output := map[string]any{
		"run_id":      linkOptRunID,
		"incident_id": linkOptIncident,
		"linked":      true,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, linkOptRunID)
		return nil
	}

	printLinkHuman(os.Stdout, output)
	return nil
}

// printLinkHuman renders the link output in a human-readable format.
func printLinkHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Run %v linked to incident %v\n", output["run_id"], output["incident_id"])
}
