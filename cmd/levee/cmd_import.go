package main

// cmd_import.go — `levee import ansible <playbook>`: translate an Ansible
// playbook into a compilable LEVEELang workflow file.
//
// TRANSLATION ONLY: the command reads the playbook, resolves every module
// through the compatibility layer (state-aware, fail-closed — unmappable
// constructs are refused with alternatives named), renders the result as
// LEVEELang YAML and proves it through the same two doors `levee compile`
// strict mode applies (parser + validator) before writing anything. It never
// executes, never touches the store, and never creates a change — the
// operator reviews the emitted file and drives it through the standard
// governance chain themselves.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/compat"
	"github.com/nexus/levee/internal/dsl"
)

var (
	importOptOut  string
	importOptName string
)

// resetImportFlags restores defaults; called from tests so repeated
// invocations do not leak flag state.
func resetImportFlags() {
	importOptOut = ""
	importOptName = ""
}

func init() {
	RegisterCommand(newImportCmd())
}

func newImportCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:   "import",
		Short: "Translate external automation formats into LEVEELang (no execution)",
	}
	parent.AddCommand(newImportAnsibleCmd())
	return parent
}

func newImportAnsibleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ansible <playbook.yaml>",
		Short: "Translate an Ansible playbook into a LEVEELang workflow",
		Long: "Translate an Ansible playbook into a LEVEELang workflow file.\n\n" +
			"Translation only: nothing is executed and no change is created. The\n" +
			"emitted YAML is proven through the same parser + validator as\n" +
			"`levee compile` before it is written, so the output is guaranteed\n" +
			"compilable. Unmappable constructs (the ansible `file`/`group` modules,\n" +
			"unknown states, ...) are refused with the alternatives named.\n\n" +
			"Output goes to stdout unless --out is given.",
		Args: cobra.ExactArgs(1),
		RunE: runImportAnsible,
	}
	cmd.Flags().StringVar(&importOptOut, "out", "", "write the workflow to this file instead of stdout")
	cmd.Flags().StringVar(&importOptName, "name", "", "override the workflow name (default: the first named play, else the file base name)")
	return cmd
}

func runImportAnsible(cmd *cobra.Command, args []string) error {
	path := args[0]
	wf, err := compat.NewAnsiblePlaybookImporter().Import(path)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	if importOptName != "" {
		wf.Meta.Name = importOptName
	}
	if wf.Meta.Name == "" {
		// The parser requires a name (LE002); derive a stable one from the
		// file so the emitted workflow is compilable without an extra flag.
		base := filepath.Base(path)
		wf.Meta.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}

	out, err := dsl.MarshalWorkflow(wf)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	// Prove the output before writing it: a translation tool that emits
	// uncompilable YAML is worse than useless.
	reparsed, err := dsl.NewParser().ParseBytes(out)
	if err != nil {
		return fmt.Errorf("import: emitted workflow does not parse (internal bug): %w", err)
	}
	if verrs := dsl.NewValidator().Validate(reparsed); len(verrs) > 0 {
		return fmt.Errorf("import: emitted workflow fails validation (internal bug): %v", verrs[0])
	}
	// A translated playbook carries the governance blocks the playbook never
	// had, so this fires on essentially every import. It goes to stderr on
	// purpose: the YAML on stdout is the command's deliverable, and "your new
	// workflow has no window, no approval tier and one batch" is the one thing
	// the operator must not have to discover at the first blocked rollout.
	emitAdvisories(cmd.ErrOrStderr(), path, dsl.NewValidator().Advise(reparsed))

	if importOptOut != "" {
		// 0o600: the emitted workflow carries hostnames, paths and parameters;
		// the operator can relax it when committing the file to a repo.
		if werr := os.WriteFile(importOptOut, out, 0o600); werr != nil {
			return fmt.Errorf("import: write %s: %w", importOptOut, werr)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d target(s), %d step(s))\n",
			importOptOut, len(reparsed.Targets), len(reparsed.Steps))
		return nil
	}
	_, err = cmd.OutOrStdout().Write(out)
	return err
}
