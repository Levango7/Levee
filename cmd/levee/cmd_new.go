package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/template"
)

// newOptParams holds the value of the --params flag for the new command.
var newOptParams string

func init() {
	RegisterCommand(newNewCmd())
}

// newNewCmd builds the `levee new` sub-command.
func newNewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new <template>",
		Short: "Instantiate a workflow from a template",
		Long: "Instantiate a workflow from a named template by filling its " +
			"placeholders with the supplied parameters. The result is a new " +
			"run record in draft status ready for planning and approval.",
		Args: cobra.ExactArgs(1),
		RunE: runNew,
	}
	cmd.Flags().StringVar(&newOptParams, "params", "", "Parameters as key=val,key2=val2")
	return cmd
}

// runNew executes the `levee new <template> --params ...` command.
func runNew(cmd *cobra.Command, args []string) error {
	templateName := args[0]

	// 1. Load configuration.
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 2. Open the template library and load the template.
	lib, err := template.NewTemplateLibrary(templateDir(cfg))
	if err != nil {
		return fmt.Errorf("open template library: %w", err)
	}
	ctx := context.Background()
	tmpl, err := lib.Get(ctx, templateName)
	if err != nil {
		return fmt.Errorf("load template %q: %w", templateName, err)
	}

	// 3. Parse the --params flag value.
	params, err := template.ParseParams(newOptParams)
	if err != nil {
		return fmt.Errorf("parse params: %w [exit=2]", err)
	}

	// 4. Instantiate the template.
	inst := template.NewInstantiator()
	result, err := inst.Instantiate(tmpl, params)
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}

	// 5. Open the state store and create a run record.
	store, err := openStore(ctx)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	runID, err := generateRunID()
	if err != nil {
		return fmt.Errorf("generate run id: %w", err)
	}

	paramsJSON, err := json.Marshal(result.Params)
	if err != nil {
		return fmt.Errorf("marshal params: %w", err)
	}

	now := time.Now().UTC()
	run := &state.Run{
		ID:             runID,
		WorkflowName:   result.TemplateName,
		TemplateName:   result.TemplateName,
		Params:         string(paramsJSON),
		Status:         "draft",
		ApprovalStatus: "pending",
		CreatedAt:      now,
		UpdatedAt:      now,
		Creator:        currentActor(),
	}
	if err := store.CreateRun(ctx, run); err != nil {
		return fmt.Errorf("create run: %w", err)
	}

	// 6. Output the result.
	output := map[string]any{
		"run_id":        runID,
		"template_name": result.TemplateName,
		"content":       result.Content,
		"params":        result.Params,
		"status":        run.Status,
		"created_at":    run.CreatedAt,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, runID)
		return nil
	}

	PrintHuman(os.Stdout, output)
	return nil
}
