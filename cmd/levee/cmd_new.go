package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/dsl"
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
			"run record in draft status ready for planning and approval.\n\n" +
			"The rendered workflow is stored on the run as its workflow source, " +
			"which is what `levee plan` later parses. Before the run is created " +
			"the rendered text must pass the same parse and structural-validation " +
			"gates `levee compile --strict` runs, and no {{.placeholder}} may " +
			"survive substitution; a template that fails any of them is refused " +
			"here (exit 2) and no run record is created.",
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
	applySecurityConfig(cfg)

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

	// 5. Refuse to mint a run this command cannot later plan. This is the first
	// moment the rendered text exists and the last one where the cause is
	// obvious, so it is where the check belongs (see validateRenderedWorkflow).
	if err := validateRenderedWorkflow(templateName, result.Content); err != nil {
		return err
	}

	// 6. Open the state store and create a run record.
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
	// WorkflowName carries the run's workflow *source*, not a display name:
	// wiring.resolveWorkflow parses it as inline YAML (or reads it as a path),
	// and this is what the gRPC instantiation path already stores. Putting the
	// template name here instead left every `levee new` run unplannable.
	run := &state.Run{
		ID:             runID,
		WorkflowName:   result.Content,
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

	// 7. Output the result.
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

// validateRenderedWorkflow checks instantiated template content with the first
// two gates `levee compile --strict` runs — parse, then structural validation.
// The third gate (strict type checking) is deliberately left out: it resolves
// arg types for known actions, so a placeholder standing in a typed position
// would be refused before any value exists, while unknown actions are skipped
// by that checker anyway (typechecker.go:184-208) — the gate would cost false
// refusals and buy nothing this check does not already cover. A placeholder
// check comes first, because both retained gates accept `{{.package}}` as a
// legal YAML string.
//
// It exists because the run record stores this exact text as its workflow
// source, which plan then parses. Before this check a template could render a
// document nothing could plan, and the failure surfaced later as
// `LE001: cannot unmarshal !!str 'probe' into dsl.yamlWorkflowRaw` — an error
// naming the run id and the workflow, with no mention of the template that was
// the cause.
func validateRenderedWorkflow(templateName, content string) error {
	if left := template.UnsubstitutedPlaceholders(content); len(left) > 0 {
		return fmt.Errorf("template %q leaves parameter(s) unsubstituted: %s — pass them with --params or declare a default in the template [exit=2]",
			templateName, strings.Join(left, ", "))
	}
	wf, err := dsl.NewParser().ParseBytes([]byte(content))
	if err != nil {
		return fmt.Errorf("template %q renders a workflow that does not parse: %w [exit=2]", templateName, err)
	}
	if verrs := dsl.NewValidator().Validate(wf); len(verrs) > 0 {
		msgs := make([]string, 0, len(verrs))
		for _, ve := range verrs {
			msgs = append(msgs, ve.Error())
		}
		return fmt.Errorf("template %q renders a workflow that fails validation: %s [exit=2]",
			templateName, strings.Join(msgs, "; "))
	}
	return nil
}
