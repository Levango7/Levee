package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/template"
	"github.com/spf13/cobra"
)

func init() {
	RegisterCommand(newCloneCmd())
}

// newCloneCmd builds the `levee clone` sub-command.
func newCloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <run-id>",
		Short: "Clone a historical change run into an editable draft",
		Long: "Clone an existing historical run into an editable draft copy. " +
			"The clone preserves the original run's parameters, batch structure " +
			"and step definitions but starts with a fresh execution history " +
			"(no trace, no audit, no approval records). The cloned run gets a " +
			"new ID and status \"draft\".",
		Args: cobra.ExactArgs(1),
		RunE: runClone,
	}
	return cmd
}

// runClone executes the `levee clone <run-id>` command.
func runClone(cmd *cobra.Command, args []string) error {
	cloneOptRunID := args[0]

	ctx := context.Background()

	// 1. Open the state store.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// 2. Clone the run.
	cloner := template.NewRunCloner(store)
	result, err := cloner.Clone(ctx, cloneOptRunID, currentActor())
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	// 3. Output the result.
	output := map[string]any{
		"original_run_id": result.OriginalRunID,
		"cloned_run_id":   result.ClonedRunID,
		"cloned_at":       result.ClonedAt,
		"cloned_by":       result.ClonedBy,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, result.ClonedRunID)
		return nil
	}

	PrintHuman(os.Stdout, output)
	return nil
}
