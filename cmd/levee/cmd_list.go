package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/state"
	"github.com/spf13/cobra"
)

// listOptStatus holds the value of the --status flag for the list command.
var listOptStatus string

// listOptTemplate holds the value of the --template flag for the list command.
var listOptTemplate string

// listOptLimit holds the value of the --limit flag for the list command.
var listOptLimit int

// listOptOffset holds the value of the --offset flag for the list command.
var listOptOffset int

func init() {
	RegisterCommand(newListCmd())
}

// newListCmd builds the `levee list` sub-command.
func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List change runs with optional filtering and pagination",
		Long: "List change runs, optionally filtered by status and template " +
			"name. Supports pagination with --limit and --offset.",
		Args: cobra.NoArgs,
		RunE: runList,
	}
	cmd.Flags().StringVar(&listOptStatus, "status", "", "Filter by status (e.g. running, completed, draft)")
	cmd.Flags().StringVar(&listOptTemplate, "template", "", "Filter by template name")
	cmd.Flags().IntVar(&listOptLimit, "limit", 20, "Maximum number of runs to return")
	cmd.Flags().IntVar(&listOptOffset, "offset", 0, "Number of runs to skip (pagination)")
	return cmd
}

// runList executes the `levee list` command.
func runList(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Build the filter from flags.
	filter := state.RunFilter{
		Status:       listOptStatus,
		TemplateName: listOptTemplate,
		Limit:        listOptLimit,
	}

	// 3. List runs.
	runs, err := store.ListRuns(ctx, filter)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}

	// 4. Build output rows.
	rows := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		rows = append(rows, map[string]any{
			"id":              r.ID,
			"workflow_name":   r.WorkflowName,
			"template_name":   r.TemplateName,
			"status":          r.Status,
			"approval_status": r.ApprovalStatus,
			"creator":         r.Creator,
			"created_at":      r.CreatedAt.Format("2006-01-02 15:04:05"),
		})
	}

	meta := map[string]any{
		"limit":  listOptLimit,
		"offset": listOptOffset,
		"count":  len(rows),
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  rows,
			"meta":  meta,
			"error": nil,
		})
	}

	if optQuiet {
		for _, r := range runs {
			fmt.Fprintln(os.Stdout, r.ID)
		}
		return nil
	}

	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "No runs found.")
		return nil
	}

	PrintHuman(os.Stdout, rows)
	return nil
}
