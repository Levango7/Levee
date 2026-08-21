package compat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// --- test helpers ----------------------------------------------------------

// newAssessor returns a fresh RiskAssessor for use in tests.
func newAssessor() *RiskAssessor {
	return NewRiskAssessor()
}

// step is a fluent helper that builds a dsl.Step with the given module and
// action. Optional modifiers can set Args, Rollback, etc.
func step(name, module, action string, mods ...func(*dsl.Step)) dsl.Step {
	s := dsl.Step{Name: name, Module: module, Action: action}
	for _, m := range mods {
		m(&s)
	}
	return s
}

// withArgs sets the step's Args map.
func withArgs(args map[string]any) func(*dsl.Step) {
	return func(s *dsl.Step) { s.Args = args }
}

// withRollback attaches a non-nil RollbackSpec to the step, signalling that
// the step has a recovery plan.
func withRollback() func(*dsl.Step) {
	return func(s *dsl.Step) { s.Rollback = &dsl.RollbackSpec{Strategy: "undo-action"} }
}

// workflow builds a *dsl.Workflow with the given name and steps.
func workflow(name string, steps ...dsl.Step) *dsl.Workflow {
	return &dsl.Workflow{
		Meta:  dsl.WorkflowMeta{Name: name},
		Steps: steps,
	}
}

// findingType returns the set of risk types present in findings.
func findingTypes(findings []RiskFinding) []RiskType {
	types := make([]RiskType, 0, len(findings))
	for _, f := range findings {
		types = append(types, f.RiskType)
	}
	return types
}

// containsRiskType reports whether findings contain a finding of the given type.
func containsRiskType(findings []RiskFinding, want RiskType) bool {
	for _, f := range findings {
		if f.RiskType == want {
			return true
		}
	}
	return false
}

// --- RiskLevel.String ------------------------------------------------------

// TestRiskLevelString verifies the String() representation of each risk level.
func TestRiskLevelString(t *testing.T) {
	assert.Equal(t, "low", RiskLow.String())
	assert.Equal(t, "medium", RiskMedium.String())
	assert.Equal(t, "high", RiskHigh.String())
	// An out-of-range level should produce a labelled unknown rather than "".
	assert.Contains(t, RiskLevel(99).String(), "unknown")
}

// --- RiskType.String -------------------------------------------------------

// TestRiskTypeString verifies the String() representation of each risk type.
func TestRiskTypeString(t *testing.T) {
	assert.Equal(t, "non-idempotent", RiskNonIdempotent.String())
	assert.Equal(t, "ignore-errors", RiskIgnoreErrors.String())
	assert.Equal(t, "no-rollback", RiskNoRollback.String())
	assert.Equal(t, "irreversible", RiskIrreversible.String())
	assert.Contains(t, RiskType(99).String(), "unknown")
}

// --- NewRiskAssessor -------------------------------------------------------

// TestNewRiskAssessor verifies that the constructor returns a non-nil assessor.
func TestNewRiskAssessor(t *testing.T) {
	a := newAssessor()
	require.NotNil(t, a)
}

// --- Assess basic ----------------------------------------------------------

// TestAssessBasic verifies that Assess returns a non-nil RiskReport with the
// workflow name propagated (req #1).
func TestAssessBasic(t *testing.T) {
	a := newAssessor()
	wf := workflow("deploy", step("read-only", "probe", "http"))
	r := a.Assess(wf)

	require.NotNil(t, r)
	assert.Equal(t, "deploy", r.WorkflowName)
	assert.NotNil(t, r.Findings)
	assert.NotEmpty(t, r.Summary)
}

// --- shell non-idempotent --------------------------------------------------

// TestAssessShellNonIdempotent verifies that a shell.exec step is flagged as
// non-idempotent at medium risk (req #2).
func TestAssessShellNonIdempotent(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("run-script", "shell", "exec", withArgs(map[string]any{"cmd": "rm -f /tmp/x"})))
	r := a.Assess(wf)

	require.NotEmpty(t, r.Findings)
	assert.True(t, containsRiskType(r.Findings, RiskNonIdempotent))

	// Locate the finding and check its level + detail.
	var f RiskFinding
	for _, cand := range r.Findings {
		if cand.RiskType == RiskNonIdempotent {
			f = cand
		}
	}
	assert.Equal(t, RiskMedium, f.RiskLevel)
	assert.Equal(t, "shell", f.Module)
	assert.Equal(t, "exec", f.Action)
	assert.Contains(t, f.Detail, "non-idempotent")
}

// --- command non-idempotent (via importer) ---------------------------------

// TestAssessCommandNonIdempotent verifies that an Ansible "command" module
// (which the importer maps to shell.exec) is detected as non-idempotent
// after import (req #3).
func TestAssessCommandNonIdempotent(t *testing.T) {
	pb := []byte(`---
- hosts: all
  tasks:
    - name: run-command
      command: /usr/bin/migrate.sh
`)
	wf, err := NewAnsiblePlaybookImporter().ImportBytes(pb)
	require.NoError(t, err)

	r := newAssessor().Assess(wf)
	assert.True(t, containsRiskType(r.Findings, RiskNonIdempotent),
		"want a non-idempotent finding for command module, got %v", r.Findings)
}

// --- ignore_errors ---------------------------------------------------------

// TestAssessIgnoreErrors verifies that a step with ignore_errors: true is
// flagged at medium risk (req #4).
func TestAssessIgnoreErrors(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("flaky", "probe", "http",
		withArgs(map[string]any{"ignore_errors": true})))
	r := a.Assess(wf)

	require.True(t, containsRiskType(r.Findings, RiskIgnoreErrors))
	for _, f := range r.Findings {
		if f.RiskType == RiskIgnoreErrors {
			assert.Equal(t, RiskMedium, f.RiskLevel)
			assert.Contains(t, f.Detail, "ignore_errors")
		}
	}
}

// TestAssessIgnoreErrorsString verifies that ignore_errors given as the
// string "true" (a YAML parsing variant) is also detected.
func TestAssessIgnoreErrorsString(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("flaky", "probe", "http",
		withArgs(map[string]any{"ignore_errors": "true"})))
	r := a.Assess(wf)
	assert.True(t, containsRiskType(r.Findings, RiskIgnoreErrors))
}

// TestAssessIgnoreErrorsFalse verifies that ignore_errors: false does NOT
// produce an ignore-errors finding.
func TestAssessIgnoreErrorsFalse(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("safe", "probe", "http",
		withArgs(map[string]any{"ignore_errors": false})))
	r := a.Assess(wf)
	assert.False(t, containsRiskType(r.Findings, RiskIgnoreErrors))
}

// --- no rollback write -----------------------------------------------------

// TestAssessNoRollbackWrite verifies that a write action without a rollback
// plan is flagged at high risk (req #5).
func TestAssessNoRollbackWrite(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("copy-config", "file", "copy",
		withArgs(map[string]any{"src": "x", "dest": "y"})))
	r := a.Assess(wf)

	require.True(t, containsRiskType(r.Findings, RiskNoRollback))
	for _, f := range r.Findings {
		if f.RiskType == RiskNoRollback {
			assert.Equal(t, RiskHigh, f.RiskLevel)
		}
	}
}

// TestAssessWriteWithRollback verifies that a write action WITH a rollback
// plan does NOT produce a no-rollback finding.
func TestAssessWriteWithRollback(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("copy-config", "file", "copy", withRollback()))
	r := a.Assess(wf)
	assert.False(t, containsRiskType(r.Findings, RiskNoRollback))
}

// --- irreversible ----------------------------------------------------------

// TestAssessIrreversible verifies that an irreversible action (pkg.remove) is
// flagged at high risk (req #6), even when a rollback is present.
func TestAssessIrreversible(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("purge-pkg", "pkg", "remove",
		withArgs(map[string]any{"name": "nginx"}), withRollback()))
	r := a.Assess(wf)

	require.True(t, containsRiskType(r.Findings, RiskIrreversible))
	for _, f := range r.Findings {
		if f.RiskType == RiskIrreversible {
			assert.Equal(t, RiskHigh, f.RiskLevel)
			assert.Contains(t, f.Detail, "irreversible")
		}
	}
	// An irreversible action should not also be flagged as no-rollback.
	assert.False(t, containsRiskType(r.Findings, RiskNoRollback))
}

// --- overall risk levels ----------------------------------------------------

// TestAssessOverallRiskHigh verifies that a high-severity finding raises the
// overall risk to high (req #7).
func TestAssessOverallRiskHigh(t *testing.T) {
	a := newAssessor()
	wf := workflow("w",
		step("probe", "probe", "http"),
		step("purge", "pkg", "remove"),
	)
	r := a.Assess(wf)
	assert.Equal(t, RiskHigh, r.OverallRisk)
}

// TestAssessOverallRiskMedium verifies that with only medium-severity
// findings the overall risk is medium (req #8).
func TestAssessOverallRiskMedium(t *testing.T) {
	a := newAssessor()
	wf := workflow("w",
		step("probe", "probe", "http"),
		step("run", "shell", "exec", withRollback()),
	)
	r := a.Assess(wf)
	// shell.exec is non-idempotent (medium); with rollback it has no
	// no-rollback finding, so the only finding is medium.
	assert.False(t, containsRiskType(r.Findings, RiskNoRollback))
	assert.Equal(t, RiskMedium, r.OverallRisk)
}

// TestAssessOverallRiskLow verifies that a workflow with no risky steps has
// overall risk low (req #9).
func TestAssessOverallRiskLow(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("probe", "probe", "http"))
	r := a.Assess(wf)
	assert.Empty(t, r.Findings)
	assert.Equal(t, RiskLow, r.OverallRisk)
}

// --- multiple steps --------------------------------------------------------

// TestAssessMultipleSteps verifies that every step is assessed and findings
// appear in step order (req #10).
func TestAssessMultipleSteps(t *testing.T) {
	a := newAssessor()
	wf := workflow("w",
		step("s1-shell", "shell", "exec"),
		step("s2-copy", "file", "copy"),
		step("s3-probe", "probe", "http"),
		step("s4-remove", "pkg", "remove"),
	)
	r := a.Assess(wf)

	// s1: non-idempotent (medium)
	// s2: no-rollback write (high)
	// s3: nothing
	// s4: irreversible (high)
	// Total: 3 findings.
	assert.Len(t, r.Findings, 3)

	// Findings should preserve step order: s1, s2, s4.
	assert.Equal(t, "s1-shell", r.Findings[0].StepName)
	assert.Equal(t, "s2-copy", r.Findings[1].StepName)
	assert.Equal(t, "s4-remove", r.Findings[2].StepName)
}

// --- empty / nil workflow --------------------------------------------------

// TestAssessNilWorkflow verifies that Assess on a nil workflow returns a
// non-nil, low-risk report rather than panicking (req #11).
func TestAssessNilWorkflow(t *testing.T) {
	a := newAssessor()
	r := a.Assess(nil)

	require.NotNil(t, r)
	assert.Equal(t, RiskLow, r.OverallRisk)
	assert.Empty(t, r.Findings)
	assert.Contains(t, r.Summary, "empty workflow")
}

// TestAssessEmptySteps verifies that a workflow with no steps yields a
// low-risk, finding-free report.
func TestAssessEmptySteps(t *testing.T) {
	a := newAssessor()
	r := a.Assess(workflow("empty"))
	require.NotNil(t, r)
	assert.Equal(t, RiskLow, r.OverallRisk)
	assert.Empty(t, r.Findings)
}

// --- summary ---------------------------------------------------------------

// TestAssessSummaryNoFindings verifies the summary text when there are no
// findings (req #12).
func TestAssessSummaryNoFindings(t *testing.T) {
	a := newAssessor()
	r := a.Assess(workflow("safe", step("probe", "probe", "http")))
	assert.Contains(t, r.Summary, "no risks detected")
	assert.Contains(t, r.Summary, "safe")
}

// TestAssessSummaryWithFindings verifies the summary text includes counts
// and the overall risk level.
func TestAssessSummaryWithFindings(t *testing.T) {
	a := newAssessor()
	wf := workflow("risky",
		step("shell", "shell", "exec"),
		step("remove", "pkg", "remove"),
	)
	r := a.Assess(wf)
	assert.Contains(t, r.Summary, "risky")
	assert.Contains(t, r.Summary, "finding")
	assert.Contains(t, r.Summary, "high")
}

// --- combined finding per step ---------------------------------------------

// TestAssessMultipleFindingsPerStep verifies that a single step can produce
// multiple findings (e.g. non-idempotent + ignore_errors).
func TestAssessMultipleFindingsPerStep(t *testing.T) {
	a := newAssessor()
	wf := workflow("w", step("flaky-shell", "shell", "exec",
		withArgs(map[string]any{"cmd": "x", "ignore_errors": true})))
	r := a.Assess(wf)

	types := findingTypes(r.Findings)
	assert.Contains(t, types, RiskNonIdempotent)
	assert.Contains(t, types, RiskIgnoreErrors)
	assert.Len(t, r.Findings, 2)
}

// --- integration with AnsiblePlaybookImporter ------------------------------

// TestAssessAfterImport verifies the end-to-end flow: import an Ansible
// playbook and then assess its risk (req #15). The sample playbook contains
// an apt install (write, no rollback -> high) and a shell (non-idempotent ->
// medium), so the overall risk should be high.
func TestAssessAfterImport(t *testing.T) {
	pb := []byte(samplePlaybook) // from compat_test.go
	wf, err := NewAnsiblePlaybookImporter().ImportBytes(pb)
	require.NoError(t, err)

	r := newAssessor().Assess(wf)
	require.NotNil(t, r)

	// The sample playbook has:
	//   - apt install nginx  -> pkg.install (write, no rollback) -> high
	//   - service start      -> svc.manage (write, no rollback) -> high
	//   - shell /opt/deploy  -> shell.exec (non-idempotent)     -> medium
	assert.True(t, containsRiskType(r.Findings, RiskNonIdempotent))
	assert.True(t, containsRiskType(r.Findings, RiskNoRollback))
	assert.Equal(t, RiskHigh, r.OverallRisk)
}

// TestAssessAfterImportCommandModule verifies that a command-module task is
// assessed as non-idempotent after import.
func TestAssessAfterImportCommandModule(t *testing.T) {
	pb := []byte(`---
- hosts: all
  tasks:
    - name: cmd-task
      command: echo hi
`)
	wf, err := NewAnsiblePlaybookImporter().ImportBytes(pb)
	require.NoError(t, err)

	r := newAssessor().Assess(wf)
	assert.True(t, containsRiskType(r.Findings, RiskNonIdempotent))
}
