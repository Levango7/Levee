package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/template"
)

// Template command option variables.
var (
	tplListOptTag       string
	tplShowOptName      string
	tplCreateOptName    string
	tplCreateOptDesc    string
	tplCreateOptContent string
	tplCreateOptParams  string
	tplDeleteOptName    string
)

func init() {
	RegisterCommand(newTemplateCmd())
}

// newTemplateCmd builds the `levee template` sub-command with its children.
func newTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "template",
		Short: "Manage workflow templates",
		Long: "Manage the LEVEE template library: list, show, create and " +
			"delete parameterized workflow templates.",
	}

	cmd.AddCommand(newTemplateListCmd())
	cmd.AddCommand(newTemplateShowCmd())
	cmd.AddCommand(newTemplateCreateCmd())
	cmd.AddCommand(newTemplateDeleteCmd())

	return cmd
}

// newTemplateListCmd builds the `levee template list` sub-command.
func newTemplateListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all templates in the library",
		Long:  "List all workflow templates in the library, sorted by name.",
		Args:  cobra.NoArgs,
		RunE:  runTemplateList,
	}
	cmd.Flags().StringVar(&tplListOptTag, "tag", "", "Filter templates by tag (key=value)")
	return cmd
}

// newTemplateShowCmd builds the `levee template show <name>` sub-command.
func newTemplateShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show detailed information about a template",
		Long:  "Display the full definition of a template including its content and parameters.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplateShow,
	}
	return cmd
}

// newTemplateCreateCmd builds the `levee template create` sub-command.
func newTemplateCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new workflow template",
		Long:  "Create a new parameterized workflow template in the library.",
		Args:  cobra.NoArgs,
		RunE:  runTemplateCreate,
	}
	cmd.Flags().StringVar(&tplCreateOptName, "name", "", "Template name (required)")
	cmd.Flags().StringVar(&tplCreateOptDesc, "description", "", "Template description")
	cmd.Flags().StringVar(&tplCreateOptContent, "content", "", "Template YAML content (required)")
	cmd.Flags().StringVar(&tplCreateOptParams, "params", "", "Template parameters as JSON array")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("content")
	return cmd
}

// newTemplateDeleteCmd builds the `levee template delete <name>` sub-command.
func newTemplateDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a workflow template",
		Long:  "Remove a template from the library by name.",
		Args:  cobra.ExactArgs(1),
		RunE:  runTemplateDelete,
	}
	return cmd
}

// openTemplateLibrary creates a TemplateLibrary from the configuration.
func openTemplateLibrary(_ctx context.Context) (*template.TemplateLibrary, error) {
	cfg, err := config.Load(optConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	dir := templateDir(cfg)
	lib, err := template.NewTemplateLibrary(dir)
	if err != nil {
		return nil, fmt.Errorf("create template library: %w", err)
	}
	return lib, nil
}

// runTemplateList executes the `levee template list` command.
func runTemplateList(cmd *cobra.Command, args []string) error {
	_ctx := context.Background()

	lib, err := openTemplateLibrary(_ctx)
	if err != nil {
		return fmt.Errorf("open template library: %w", err)
	}

	templates, err := lib.List(_ctx)
	if err != nil {
		return fmt.Errorf("list templates: %w", err)
	}

	// Filter by tag if specified.
	if tplListOptTag != "" {
		templates = filterTemplatesByTag(templates, tplListOptTag)
	}

	// Build output.
	rows := make([]map[string]any, 0, len(templates))
	for _, tmpl := range templates {
		rows = append(rows, map[string]any{
			"id":          tmpl.ID,
			"name":        tmpl.Name,
			"description": tmpl.Description,
			"created_at":  tmpl.CreatedAt,
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
		for _, tmpl := range templates {
			fmt.Fprintln(os.Stdout, tmpl.Name)
		}
		return nil
	}

	printTemplateListHuman(os.Stdout, rows)
	return nil
}

// runTemplateShow executes the `levee template show <name>` command.
func runTemplateShow(cmd *cobra.Command, args []string) error {
	tplShowOptName = args[0]
	_ctx := context.Background()

	lib, err := openTemplateLibrary(_ctx)
	if err != nil {
		return fmt.Errorf("open template library: %w", err)
	}

	tmpl, err := lib.Get(_ctx, tplShowOptName)
	if err != nil {
		return fmt.Errorf("get template: %w", err)
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  tmpl,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, tmpl.ID)
		return nil
	}

	humanStr, err := lib.Show(_ctx, tplShowOptName)
	if err != nil {
		return fmt.Errorf("show template: %w", err)
	}
	fmt.Fprint(os.Stdout, humanStr)
	return nil
}

// runTemplateCreate executes the `levee template create` command.
func runTemplateCreate(cmd *cobra.Command, args []string) error {
	_ctx := context.Background()

	lib, err := openTemplateLibrary(_ctx)
	if err != nil {
		return fmt.Errorf("open template library: %w", err)
	}

	// Parse optional parameters JSON.
	var params []template.TemplateParam
	if tplCreateOptParams != "" {
		if err := json.Unmarshal([]byte(tplCreateOptParams), &params); err != nil {
			return fmt.Errorf("parse params JSON: %w", err)
		}
	}

	tmpl := &template.Template{
		Name:        tplCreateOptName,
		Description: tplCreateOptDesc,
		Content:     tplCreateOptContent,
		Parameters:  params,
	}

	if err := lib.Save(_ctx, tmpl); err != nil {
		return fmt.Errorf("create template: %w", err)
	}

	output := map[string]any{
		"id":         tmpl.ID,
		"name":       tmpl.Name,
		"created_at": tmpl.CreatedAt,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, tmpl.ID)
		return nil
	}

	printTemplateCreateHuman(os.Stdout, output)
	return nil
}

// runTemplateDelete executes the `levee template delete <name>` command.
func runTemplateDelete(cmd *cobra.Command, args []string) error {
	tplDeleteOptName = args[0]
	_ctx := context.Background()

	lib, err := openTemplateLibrary(_ctx)
	if err != nil {
		return fmt.Errorf("open template library: %w", err)
	}

	if err := lib.Delete(_ctx, tplDeleteOptName); err != nil {
		return fmt.Errorf("delete template: %w", err)
	}

	output := map[string]any{
		"name":    tplDeleteOptName,
		"deleted": true,
	}

	if optJSON {
		return PrintJSON(os.Stdout, map[string]any{
			"data":  output,
			"meta":  nil,
			"error": nil,
		})
	}

	if optQuiet {
		fmt.Fprintln(os.Stdout, tplDeleteOptName)
		return nil
	}

	printTemplateDeleteHuman(os.Stdout, output)
	return nil
}

// filterTemplatesByTag filters templates by a "key=value" tag expression.
func filterTemplatesByTag(templates []*template.Template, tagExpr string) []*template.Template {
	parts := strings.SplitN(tagExpr, "=", 2)
	if len(parts) != 2 {
		return templates
	}
	key, val := parts[0], parts[1]

	var filtered []*template.Template
	for _, tmpl := range templates {
		if v, ok := tmpl.Tags[key]; ok && v == val {
			filtered = append(filtered, tmpl)
		}
	}
	return filtered
}

// printTemplateListHuman renders the template list in a human-readable format.
func printTemplateListHuman(w io.Writer, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "No templates found.")
		return
	}
	fmt.Fprintf(w, "%-20s %-12s %s\n", "NAME", "ID", "DESCRIPTION")
	for _, row := range rows {
		name, _ := row["name"].(string)
		id, _ := row["id"].(string)
		desc, _ := row["description"].(string)
		fmt.Fprintf(w, "%-20s %-12s %s\n", name, id, desc)
	}
}

// printTemplateCreateHuman renders the template create output.
func printTemplateCreateHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Template created: %v (id=%v)\n", output["name"], output["id"])
}

// printTemplateDeleteHuman renders the template delete output.
func printTemplateDeleteHuman(w io.Writer, output map[string]any) {
	fmt.Fprintf(w, "Template deleted: %v\n", output["name"])
}
