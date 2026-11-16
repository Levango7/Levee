package dsl

// irreversible_compensation_test.go pins LE108: an irreversible step that
// declares no compensation.
//
// The gap this closes is a visibility one, not a missing refusal. R2 says a
// change must be reversible; `irreversible: true` is how an author says "this
// one is not", and until now that declaration carried no consequence beyond
// the approval tier. The step with no way back looked exactly like a step with
// one — the difference only surfaced when the change failed halfway and there
// was nothing to run.
//
// It stays a warning on purpose: refusing an irreversible step would push the
// genuinely one-way changes (data deletion, replica swap) outside the platform,
// where nothing governs them at all. What the approver needs is to SEE which
// steps have no way back, not to be unable to submit the change.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/errors"
)

const irreversibleNoCompensation = `name: one-way
allow_irreversible:
  - shell.exec
target:
  type: host
  query: "env=test"
steps:
  - name: drop-old-logs
    action: shell.exec
    args:
      cmd: "rm -f /var/log/app/old.log"
    irreversible: true
`

const irreversibleWithCompensation = `name: one-way-with-undo
allow_irreversible:
  - shell.exec
target:
  type: host
  query: "env=test"
steps:
  - name: drop-old-logs
    action: shell.exec
    args:
      cmd: "rm -f /var/log/app/old.log"
    irreversible: true
    rollback:
      strategy: undo-action
      step:
        name: restore-old-logs
        action: shell.exec
        args:
          cmd: "touch /var/log/app/old.log"
        idempotent: true
`

const irreversibleSnapshotChoice = `name: one-way-snapshot
allow_irreversible:
  - shell.exec
target:
  type: host
  query: "env=test"
steps:
  - name: drop-old-logs
    action: shell.exec
    args:
      cmd: "rm -f /var/log/app/old.log"
    irreversible: true
    rollback:
      strategy: snapshot
      snapshot_paths:
        - /var/log/app/old.log
`

// The inherent list is the second half of "irreversible": an author who does not
// write `irreversible: true` on `pkg.remove` still gets the advisory, because
// the catalogue already treats that action as destructive.
const inherentIrreversibleNoCompensation = `name: inherent-one-way
allow_irreversible:
  - pkg.remove
target:
  type: host
  query: "env=test"
steps:
  - name: purge-package
    action: pkg.remove
    args:
      name: telnet
`

func TestAdvise_IrreversibleWithoutCompensation(t *testing.T) {
	codes := adviseCodes(t, irreversibleNoCompensation)
	assert.Contains(t, codes, codeIrreversibleWithoutCompensation,
		"a step that admits it is irreversible and declares no way back must be flagged")
}

func TestAdvise_InherentlyIrreversibleWithoutCompensation(t *testing.T) {
	codes := adviseCodes(t, inherentIrreversibleNoCompensation)
	assert.Contains(t, codes, codeIrreversibleWithoutCompensation,
		"pkg.remove is inherently irreversible; not writing the flag does not make it reversible")
}

func TestAdvise_NoAdvisoryWhenACompensationIsDeclared(t *testing.T) {
	codes := adviseCodes(t, irreversibleWithCompensation)
	assert.NotContains(t, codes, codeIrreversibleWithoutCompensation)
}

// A strategy-only declaration is a real compensation (a snapshot restore is one
// instruction, not a list of undo steps). Reporting it would teach authors to
// add empty undo steps just to silence the advisory.
func TestAdvise_SnapshotStrategyCountsAsACompensation(t *testing.T) {
	codes := adviseCodes(t, irreversibleSnapshotChoice)
	assert.NotContains(t, codes, codeIrreversibleWithoutCompensation)
}

// The advisory must not fire on ordinary steps: a reversible step with no
// rollback is the normal case (the platform's compensation ledger is
// per-step opt-in), and flagging it would bury the irreversible ones.
func TestAdvise_ReversibleStepIsNotAdvised(t *testing.T) {
	codes := adviseCodes(t, minimalWorkflow)
	assert.NotContains(t, codes, codeIrreversibleWithoutCompensation)
}

// An advisory never blocks. If this ever fails, the "warning" classification in
// the catalogue and the emission here have drifted apart.
func TestAdvise_IrreversibleAdvisoryDoesNotBlock(t *testing.T) {
	wf := mustAdviseParse(t, irreversibleNoCompensation)
	// The fixture is whitelisted (LE082 satisfied), so the ONLY thing standing
	// between it and a plan is the advisory — which must not stand in the way.
	errs := NewValidator().Validate(wf)
	require.Empty(t, errs, "a whitelisted irreversible step with no compensation must compile")
	// And the catalogue agrees it is a warning, not an error.
	ci, ok := errors.Lookup(errors.LE108)
	require.True(t, ok, "LE108 must be registered in the catalogue")
	assert.Equal(t, errors.CompileWarning, ci.Compile)
}

// One finding per step, sorted: a workflow with several one-way steps gets
// several actionable lines, not one that names a list the author has to
// cross-reference.
func TestAdvise_OneFindingPerIrreversibleStepSorted(t *testing.T) {
	src := `name: two-one-way
allow_irreversible:
  - shell.exec
target:
  type: host
  query: "env=test"
steps:
  - name: zeta-drop
    action: shell.exec
    args:
      cmd: "rm -f /tmp/z"
    irreversible: true
  - name: alpha-drop
    action: shell.exec
    args:
      cmd: "rm -f /tmp/a"
    irreversible: true
`
	wf := mustAdviseParse(t, src)
	var fields []string
	for _, a := range NewValidator().Advise(wf) {
		if a.Code == codeIrreversibleWithoutCompensation {
			fields = append(fields, a.Field)
		}
	}
	assert.Equal(t, []string{"steps.alpha-drop", "steps.zeta-drop"}, fields)
}
