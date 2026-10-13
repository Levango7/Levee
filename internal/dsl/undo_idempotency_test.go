// Tests for LE103, the advisory that fires when a workflow's undo step does
// not declare `idempotent: true`.
//
// Closes the last step of the idempotent gate (方案 C): the gate is passive —
// it validates what an author declares — and this Lint is the active half that
// tells the author what they FORGOT to declare. The roadmap row names it
// "从被动门禁变主动信息".
//
// The finding it pins: compensation is the path that runs when something has
// already gone wrong, so it is the path most likely to be re-run. A partially
// applied forward batch leaves the executor retrying the undo, and a
// non-idempotent undo run twice does its work twice — "append the removed
// line" duplicates it, "delete the created file" fails the second time and
// surfaces as a compensation error that masks the original failure. Neither
// shows up in the first run, which is why nothing caught it before.
//
// It is a warning, not an error, for the same reason Advise exists at all: the
// author may know the undo is naturally idempotent (a snapshot restore, a
// config revert), and forcing the declaration would break documents that
// compile today. Every case below is a shape an author actually writes.
package dsl

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/errors"
)

// undoIdempotencyFor runs Advise over src and returns the LE103 findings.
func undoIdempotencyFor(t *testing.T, src string) []ValidationError {
	t.Helper()
	wf, err := NewParser().ParseBytes([]byte(src))
	require.NoError(t, err, "the fixture must parse: the advisory is about semantics, not syntax")
	var out []ValidationError
	for _, a := range NewValidator().Advise(wf) {
		if a.Code == codeNonIdempotentUndo {
			out = append(out, a)
		}
	}
	return out
}

// workflowWithUndo wraps a rollback block into a complete document, because
// Advise runs on a parsed workflow and the parser needs target + steps.
func workflowWithUndo(rollbackYAML string) string {
	return fmt.Sprintf(`name: adv-undo-idem
target:
  type: host
  query: "env=test"
steps:
  - name: deploy
    action: shell.exec
    args:
      cmd: "true"
%s
`, rollbackYAML)
}

func TestUndoIdempotencyAdvisoryFiresOnUndeclaredUndo(t *testing.T) {
	src := workflowWithUndo(`    rollback:
      strategy: undo-action
      steps:
        - name: restore
          action: shell.exec
          args:
            cmd: "true"
`)
	found := undoIdempotencyFor(t, src)
	require.Len(t, found, 1, "an undo step with no idempotent declaration is the finding")
	assert.Equal(t, "steps.deploy.rollback", found[0].Field, "the field points at the step the author edits")
	assert.Contains(t, found[0].Message, "idempotent: true", "the message names the declaration it wants")
	assert.Contains(t, found[0].Message, "deploy", "and the step it belongs to")
}

func TestUndoIdempotencyAdvisorySilentWhenDeclaredOnUndoStep(t *testing.T) {
	src := workflowWithUndo(`    rollback:
      strategy: undo-action
      steps:
        - name: restore
          action: shell.exec
          idempotent: true
          args:
            cmd: "true"
`)
	assert.Empty(t, undoIdempotencyFor(t, src), "a declared undo is the closed case")
}

func TestUndoIdempotencyAdvisorySilentWhenDeclaredOnForwardStep(t *testing.T) {
	// `idempotent: true` on the owning step covers its undo block, because that
	// is how the YAML reads: the field sits on the step, not on each of its
	// rollback.steps entries. An author who annotates the step has said "this
	// whole unit is re-runnable", undo included.
	src := `name: adv-undo-fwd-idem
target:
  type: host
  query: "env=test"
steps:
  - name: deploy
    action: shell.exec
    idempotent: true
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: restore
          action: shell.exec
          args:
            cmd: "true"
`
	assert.Empty(t, undoIdempotencyFor(t, src), "the forward step's declaration covers its undo block")
}

func TestUndoIdempotencyAdvisorySilentWithoutRollback(t *testing.T) {
	// No rollback at all is a different finding (and a fatal one, if the
	// workflow declares nothing compensable); this advisory must stay quiet
	// rather than pile on.
	src := workflowWithUndo("")
	assert.Empty(t, undoIdempotencyFor(t, src))
}

func TestUndoIdempotencyAdvisorySilentWhenUndoBlockEmpty(t *testing.T) {
	// A rollback block with no steps (an audit-only record) has nothing to
	// annotate.
	src := workflowWithUndo(`    rollback:
      strategy: config-revert
`)
	assert.Empty(t, undoIdempotencyFor(t, src))
}

func TestUndoIdempotencyAdvisoryReportsOneFindingPerStep(t *testing.T) {
	// Three sloppy undo steps, three actionable lines: one finding that names a
	// list is a finding the author has to cross-reference by hand.
	src := `name: adv-undo-multi
target:
  type: host
  query: "env=test"
steps:
  - name: alpha
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: undo-alpha
          action: shell.exec
          args:
            cmd: "true"
  - name: bravo
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: undo-bravo
          action: shell.exec
          args:
            cmd: "true"
  - name: charlie
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: undo-charlie
          action: shell.exec
          args:
            cmd: "true"
`
	found := undoIdempotencyFor(t, src)
	require.Len(t, found, 3)
	assert.Equal(t, "steps.alpha.rollback", found[0].Field)
	assert.Equal(t, "steps.bravo.rollback", found[1].Field)
	assert.Equal(t, "steps.charlie.rollback", found[2].Field)
}

func TestUndoIdempotencyAdvisoryMixedStepsNameOnlyTheSloppy(t *testing.T) {
	// One declared, one not: only the undeclared one is reported. A finding that
	// also names the compliant step trains the author to ignore the advisory.
	src := `name: adv-undo-mixed
target:
  type: host
  query: "env=test"
steps:
  - name: good
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: undo-good
          action: shell.exec
          idempotent: true
          args:
            cmd: "true"
  - name: bad
    action: shell.exec
    args:
      cmd: "true"
    rollback:
      strategy: undo-action
      steps:
        - name: undo-bad
          action: shell.exec
          args:
            cmd: "true"
`
	found := undoIdempotencyFor(t, src)
	require.Len(t, found, 1)
	assert.Equal(t, "steps.bad.rollback", found[0].Field)
}

// The gate property: the advisory must be a warning, never an error. A caller
// that combined Validate and Advise into one list and treated non-empty as
// fatal would turn this into a compile break.
func TestUndoIdempotencyAdvisoryIsWarningSeverity(t *testing.T) {
	info, ok := errors.Lookup(errors.LE103)
	require.True(t, ok, "LE103 must be registered in the catalogue")
	assert.Equal(t, errors.CompileWarning, info.Compile,
		"LE103 must be a CompileWarning: documents that compile today keep compiling")
}
