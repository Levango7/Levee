package main

// cmd_compile.go — `levee compile <file>` command.
//
// The compile command is the entry point for LEVEELang compile-time type
// checking and IR generation. It:
//  1. Parses the YAML workflow file into an AST.
//  2. Runs the basic structural validator.
//  3. Runs the compile-time type checker under the selected mode
//     (--strict default, --lenient for warnings-only).
//  4. Optionally emits the IR as JSON (--ir) or stops after type checking
//     (--check-only).
//
// All errors are reported with source file + line + column when available,
// and multiple errors are batched into a single report.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/dsl"
)

// compileOpt* are the flag values for the compile command. They are reset
// before each invocation by resetCompileFlags (called from tests) so that
// repeated calls do not leak state.
var (
	compileOptStrict    bool
	compileOptLenient   bool
	compileOptIR        bool
	compileOptCheckOnly bool
)

func init() {
	RegisterCommand(newCompileCmd())
}

// newCompileCmd builds the `levee compile <file>` sub-command.
func newCompileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compile <file>",
		Short: "Compile a LEVEELang workflow file",
		Long: "Compile a LEVEELang workflow file: parse, validate and type-check, " +
			"optionally emitting the intermediate representation (IR) as JSON.\n\n" +
			"Modes:\n" +
			"  --strict    (default) type errors are fatal\n" +
			"  --lenient   type errors are reported as warnings, IR is still emitted\n\n" +
			"Outputs:\n" +
			"  --ir            emit the IR as a JSON document on stdout\n" +
			"  --check-only    type-check only, do not generate IR",
		Args: cobra.ExactArgs(1),
		RunE: runCompile,
	}
	cmd.Flags().BoolVar(&compileOptStrict, "strict", true, "Strict mode: type errors are fatal (default)")
	cmd.Flags().BoolVar(&compileOptLenient, "lenient", false, "Lenient mode: type errors are warnings")
	cmd.Flags().BoolVar(&compileOptIR, "ir", false, "Emit the IR as JSON on stdout")
	cmd.Flags().BoolVar(&compileOptCheckOnly, "check-only", false, "Type-check only, do not generate IR")
	return cmd
}

// runCompile executes the compile command.
func runCompile(cmd *cobra.Command, args []string) error {
	file := args[0]
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// Resolve mode. --lenient wins over --strict when both are set, mirroring
	// the more permissive flag's intent.
	mode := dsl.ModeStrict
	if compileOptLenient {
		mode = dsl.ModeLenient
	}

	// 1. Parse.
	parser := dsl.NewParser()
	wf, err := parser.ParseFile(file)
	if err != nil {
		return formatCompileError(file, err)
	}

	// 2. Basic structural validation.
	validator := dsl.NewValidator()
	verrs := validator.Validate(wf)
	if len(verrs) > 0 && mode == dsl.ModeStrict {
		return formatValidationErrors(file, verrs)
	}
	// In lenient mode, validation errors are reported on stderr but do not stop.
	for _, ve := range verrs {
		fmt.Fprintln(errOut, formatValidationError(file, ve))
	}

	// 3. Type checking.
	registry := dsl.NewTypeRegistry()
	checker := dsl.NewTypeChecker(registry, file)
	terrs := checker.CheckWithMode(wf, mode)
	if len(terrs) > 0 && mode == dsl.ModeStrict {
		return formatTypeErrors(file, terrs)
	}
	// In lenient mode, type errors are warnings on stderr.
	for _, te := range terrs {
		fmt.Fprintln(errOut, formatTypeError(file, te))
	}

	// 4. --check-only stops here.
	if compileOptCheckOnly {
		emitCompileSummary(out, file, wf, len(verrs), len(terrs))
		return nil
	}

	// 5. IR generation.
	ir, err := dsl.GenerateIR(wf, registry)
	if err != nil {
		return fmt.Errorf("generate ir: %w", err)
	}

	if compileOptIR {
		return emitIRJSON(out, ir)
	}

	emitCompileSummary(out, file, wf, len(verrs), len(terrs))
	return nil
}

// emitIRJSON writes the IR as a pretty-printed JSON document to out. In --json
// mode it is wrapped in the standard output envelope.
func emitIRJSON(out io.Writer, ir *dsl.IR) error {
	if optJSON {
		return PrintJSON(out, map[string]any{
			"data":  ir,
			"meta":  map[string]any{"ir_version": dsl.IRVersion},
			"error": nil,
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ir); err != nil {
		return fmt.Errorf("encode ir: %w", err)
	}
	return nil
}

// emitCompileSummary prints a human-readable summary of the compile result.
func emitCompileSummary(out io.Writer, file string, wf *dsl.Workflow, valErrs, typeErrs int) {
	if optJSON {
		_ = PrintJSON(out, map[string]any{
			"data": map[string]any{
				"file":            file,
				"workflow":        wf.Meta.Name,
				"version":         wf.Meta.Version,
				"validation_errs": valErrs,
				"type_errs":       typeErrs,
				"ok":              valErrs == 0 && typeErrs == 0,
			},
			"meta":  nil,
			"error": nil,
		})
		return
	}
	if optQuiet {
		fmt.Fprintln(out, file)
		return
	}
	status := "ok"
	if valErrs > 0 || typeErrs > 0 {
		status = "warnings"
	}
	fmt.Fprintf(out, "compile %s: %s (workflow=%s, val_errs=%d, type_errs=%d)\n",
		file, status, wf.Meta.Name, valErrs, typeErrs)
}

// ---------------------------------------------------------------------------
// Error formatting helpers
// ---------------------------------------------------------------------------

// formatCompileError wraps a parse error with the source file location.
func formatCompileError(file string, err error) error {
	return fmt.Errorf("compile %s: %w", file, err)
}

// formatValidationErrors converts a slice of ValidationError into a single
// multi-line error suitable for CLI output.
func formatValidationErrors(file string, errs []dsl.ValidationError) error {
	var b strings.Builder
	fmt.Fprintf(&b, "compile %s: %d validation error(s):", file, len(errs))
	for _, e := range errs {
		fmt.Fprintf(&b, "\n  %s: %s", file, e.Error())
	}
	return fmt.Errorf("%s", b.String())
}

// formatValidationError formats a single validation error for stderr output.
func formatValidationError(file string, e dsl.ValidationError) string {
	return fmt.Sprintf("warning: %s: %s", file, e.Error())
}

// formatTypeErrors converts a slice of TypeError into a single multi-line
// error suitable for CLI output.
func formatTypeErrors(file string, errs []dsl.TypeError) error {
	var b strings.Builder
	fmt.Fprintf(&b, "compile %s: %d type error(s):", file, len(errs))
	for _, e := range errs {
		fmt.Fprintf(&b, "\n  %s", e.Error())
	}
	return fmt.Errorf("%s", b.String())
}

// formatTypeError formats a single type error for stderr output.
func formatTypeError(file string, e dsl.TypeError) string {
	if e.File == "" {
		e.File = file
	}
	return fmt.Sprintf("warning: %s", e.Error())
}
