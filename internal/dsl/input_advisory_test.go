// Tests for LE004, the advisory that fires when a document declares input
// parameters or references them with `{{input.x}}`.
//
// The finding this pins: the `input:` block was parsed, type-checked, lowered
// into the IR (hence into the plan hash) and documented as a parameter
// interface — and nothing in the pipeline supplies or substitutes the values.
// The reference shape appears in the spec and in fixtures, never in the planner,
// the executor or the RPC surface (there is no field or flag that carries a
// value). So an author following the spec gets the literal text passed to the
// module, which for `shell.exec` is a command nobody wrote.
//
// The advisory is the honest middle: wiring the feature is a project of its own
// (value plumbing + resolution + redaction), and deleting the syntax from the
// spec alone would leave the parser accepting it silently. Every case below is
// a shape an author actually writes.
package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/errors"
)

// inputAdvisoryFor runs Advise over src and returns the LE004 findings.
func inputAdvisoryFor(t *testing.T, src string) []ValidationError {
	t.Helper()
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err, "the fixture must parse: the advisory is about semantics, not syntax")
	var out []ValidationError
	for _, a := range NewValidator().Advise(wf) {
		if a.Code == codeUnresolvedInputs {
			out = append(out, a)
		}
	}
	return out
}

func TestInputAdvisoryFiresOnDeclarationAlone(t *testing.T) {
	// No reference anywhere: the promise itself is the finding. This is the
	// cmd/levee validCompileYAML shape.
	src := `name: adv-input-decl
input:
  - name: pkg
    type: string
    required: true
target:
  type: host
  query: "env=test"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "true"
`
	found := inputAdvisoryFor(t, src)
	require.Len(t, found, 1, "declaring an input block is the finding, reference or not")
	assert.Equal(t, "input", found[0].Field)
	assert.Contains(t, found[0].Message, "nothing supplies or substitutes them")
}

func TestInputAdvisoryFiresOnReferenceWithoutDeclaration(t *testing.T) {
	// Both spellings the spec uses appear in its own examples; both are dead.
	for _, ref := range []string{"{{input.package_name}}", "{{ input.package_name }}", "{{  input.package_name  }}"} {
		t.Run(ref, func(t *testing.T) {
			src := `name: adv-input-ref
target:
  type: host
  query: "env=test"
steps:
  - name: upgrade
    action: shell.exec
    args:
      cmd: "rpm -Uvh ` + ref + `.rpm"
`
			require.Len(t, inputAdvisoryFor(t, src), 1,
				"a reference nothing resolves must be reported even with no input block")
		})
	}
}

func TestInputAdvisoryWalksNestedArgsAndRollbackSteps(t *testing.T) {
	// args is map[string]any all the way down, and undo steps carry args too —
	// a walker that only looks at top-level strings misses both, and both are
	// how the reference reaches a module.
	t.Run("nested mapping", func(t *testing.T) {
		src := `name: adv-input-nested
target:
  type: host
  query: "env=test"
steps:
  - name: configure
    action: file.template
    args:
      dest: /etc/app.conf
      vars:
        table: "{{ input.table }}"
`
		require.Len(t, inputAdvisoryFor(t, src), 1, "a reference nested in a mapping is still a reference")
	})

	t.Run("list element", func(t *testing.T) {
		src := `name: adv-input-list
target:
  type: host
  query: "env=test"
steps:
  - name: configure
    action: shell.exec
    args:
      argv:
        - "deploy"
        - "{{input.version}}"
`
		require.Len(t, inputAdvisoryFor(t, src), 1, "a reference inside a list is still a reference")
	})

	t.Run("rollback step", func(t *testing.T) {
		src := `name: adv-input-rollback
target:
  type: host
  query: "env=test"
steps:
  - name: upgrade
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: downgrade
          action: shell.exec
          args:
            cmd: "rpm -Uvh {{ input.previous_package }}.rpm"
`
		require.Len(t, inputAdvisoryFor(t, src), 1, "undo steps are args too — the same literal reaches the module")
	})
}

func TestInputAdvisoryAcceptsTemplateInstantiationSyntax(t *testing.T) {
	// `{{.name}}` is the REAL mechanism (internal/template instantiation fills
	// it, and UnsubstitutedPlaceholders reports leftovers). Reporting it here
	// would push authors away from the path that works.
	src := `name: adv-template-syntax
target:
  type: host
  query: "env=test"
steps:
  - name: reload
    action: shell.exec
    args:
      cmd: "systemctl restart {{.service}}"
`
	assert.Empty(t, inputAdvisoryFor(t, src),
		"{{.name}} is filled by template instantiation; it must not be reported as an unresolved input")
}

func TestInputAdvisoryIsSilentOnOrdinaryWorkflows(t *testing.T) {
	// The existing exact-list test (TestAdviseFiresForUndeclaredGovernanceBlocks)
	// pins the three governance codes for minimalWorkflow; this pins that LE004
	// adds nothing there, i.e. the advisory cannot fire on a document with no
	// input block and no reference.
	assert.Empty(t, inputAdvisoryFor(t, minimalWorkflow))
}

func TestInputAdvisorySeverityComesFromTheCatalogue(t *testing.T) {
	// Same property the rest of the channel rests on: the code is a catalogue
	// warning (so it can never block), not a set restated in the dsl package.
	assert.True(t, IsCompileWarning(codeUnresolvedInputs),
		"LE004 must be catalogued as a compile warning, or it becomes a behaviour break")
	ci, ok := errors.Lookup(errors.LE004)
	require.True(t, ok, "LE004 must be registered, or the CLI cannot print it and the docs cannot describe it")
	assert.Contains(t, ci.Description, "nothing resolves them")
}
