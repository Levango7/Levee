// integration_test.go — integration tests for the compat package (T063).
//
// These tests exercise the end-to-end compatibility layer pipeline:
//   Ansible playbook YAML → ImportBytes → RiskAssess → Execute → audit trace
//
// They build on the unit tests in compat_test.go, executor_test.go and
// risk_test.go, reusing the helpers defined there (newImporter, newExecutor,
// newAssessor, newTestStore, containsRiskType). The scenarios cover the full
// pipeline, pure shell playbooks, file/copy/template playbooks, high-risk apt
// playbooks, multi-target execution, audit-trace verification, approval
// wrapping and the import+assess combination.

package compat

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- shared fixtures -------------------------------------------------------

// fullPipelinePlaybook is an Ansible playbook exercising the shell, file and
// copy modules — all of which are in the executor's supportedModules set, so
// the simulated execution succeeds end-to-end. We avoid apt/yum here because
// those map to pkg.install, which the MVP executor treats as unsupported; the
// high-risk apt scenario is covered separately by
// TestIntegration_RiskHighPlaybook.
const fullPipelinePlaybook = `---
- hosts: web_servers
  become: true
  tasks:
    - name: ensure dir
      file: path=/srv/app state=directory
    - name: copy config
      copy:
        src: nginx.conf
        dest: /etc/nginx/nginx.conf
    - name: restart nginx
      shell: systemctl restart nginx
`

// shellOnlyPlaybook is an Ansible playbook with only shell tasks. Each shell
// task is non-idempotent (RiskMedium) but performs no write, so the overall
// risk is medium.
const shellOnlyPlaybook = `---
- hosts: web_servers
  tasks:
    - name: run deploy
      shell: /opt/deploy.sh
    - name: run migrate
      shell: /opt/migrate.sh
`

// fileCopyTemplatePlaybook exercises the file, copy and template modules.
// All three are supported by the MVP executor and succeed when simulated.
const fileCopyTemplatePlaybook = `---
- hosts: app_servers
  tasks:
    - name: ensure dir
      file: path=/srv/app state=directory
    - name: push config
      copy:
        src: app.conf
        dest: /etc/app/app.conf
    - name: render template
      template:
        src: app.j2
        dest: /etc/app/app.conf
`

// aptHighRiskPlaybook installs a package via apt without a rollback plan,
// producing a RiskNoRollback finding at RiskHigh.
const aptHighRiskPlaybook = `---
- hosts: web_servers
  become: true
  tasks:
    - name: install nginx
      apt:
        name: nginx
        state: present
`

// multiTargetPlaybook runs two shell steps; used together with a multi-target
// execute call to verify target × step expansion.
const multiTargetPlaybook = `---
- hosts: all
  tasks:
    - name: step one
      shell: echo 1
    - name: step two
      shell: echo 2
`

// --- integration helpers ---------------------------------------------------

// importAndAssess imports pb and assesses its risk, returning the workflow
// and report. It fails the test on any import error.
func importAndAssess(t *testing.T, pb string) (*dsl.Workflow, *RiskReport) {
	t.Helper()
	wf, err := NewAnsiblePlaybookImporter().ImportBytes([]byte(pb))
	require.NoError(t, err)
	require.NotNil(t, wf)
	r := NewRiskAssessor().Assess(wf)
	require.NotNil(t, r)
	return wf, r
}

// countTraces counts trace records whose Event matches event.
func countTraces(traces []*state.Trace, event string) int {
	n := 0
	for _, tr := range traces {
		if tr.Event == event {
			n++
		}
	}
	return n
}

// --- tests -----------------------------------------------------------------

// TestIntegration_FullPipeline exercises the complete end-to-end flow:
// import an Ansible playbook, assess its risk, execute it against a target
// and verify that audit traces are recorded. The playbook uses only
// executor-supported modules (shell, file, copy) so the simulated run
// succeeds.
func TestIntegration_FullPipeline(t *testing.T) {
	// 1. Import + assess.
	wf, report := importAndAssess(t, fullPipelinePlaybook)
	require.Len(t, wf.Steps, 3)

	// 2. Risk assessment: shell → medium (non-idempotent), file/copy → high
	//    (write without rollback). Overall risk is high.
	assert.Equal(t, RiskHigh, report.OverallRisk)
	assert.True(t, containsRiskType(report.Findings, RiskNonIdempotent),
		"expected a non-idempotent finding for the shell step")
	assert.True(t, containsRiskType(report.Findings, RiskNoRollback),
		"expected a no-rollback finding for the file/copy steps")

	// 3. Execute against a single target.
	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, []string{"web-1"})
	require.NoError(t, err)
	require.NotNil(t, res)

	// 4. Verify execution result.
	assert.True(t, res.Success, "supported modules should all succeed")
	assert.Len(t, res.Steps, 3)
	assert.Equal(t, "web-1", res.Steps[0].Target)
	for _, s := range res.Steps {
		assert.True(t, s.Success, "step %q should succeed", s.Name)
	}

	// 5. Verify audit traces were recorded.
	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)
	assert.NotEmpty(t, traces)
	assert.Equal(t, 3, countTraces(traces, audit.EventStepExecute),
		"expected 3 step_execute traces")
}

// TestIntegration_ShellPlaybook imports a pure-shell playbook, assesses its
// risk and executes it. Shell steps are non-idempotent (medium) but perform
// no write, so the overall risk is medium and execution succeeds.
func TestIntegration_ShellPlaybook(t *testing.T) {
	wf, report := importAndAssess(t, shellOnlyPlaybook)
	require.Len(t, wf.Steps, 2)

	// Every step is shell.exec → non-idempotent (medium). No write actions,
	// so no no-rollback findings; overall risk is medium.
	assert.Equal(t, RiskMedium, report.OverallRisk)
	assert.False(t, containsRiskType(report.Findings, RiskNoRollback))
	for _, f := range report.Findings {
		assert.Equal(t, RiskMedium, f.RiskLevel)
		assert.Equal(t, RiskNonIdempotent, f.RiskType)
	}

	// Execute and verify success.
	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, []string{"web-1"})
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Len(t, res.Steps, 2)
}

// TestIntegration_FileCopyTemplate imports a playbook using the file, copy
// and template modules, then executes it. All three are supported by the MVP
// executor and succeed when simulated.
func TestIntegration_FileCopyTemplate(t *testing.T) {
	wf, report := importAndAssess(t, fileCopyTemplatePlaybook)
	require.Len(t, wf.Steps, 3)

	// file.manage, file.copy and file.template are all write actions without
	// a rollback plan → high risk.
	assert.Equal(t, RiskHigh, report.OverallRisk)

	// Execute and verify all steps succeed.
	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, []string{"app-1"})
	require.NoError(t, err)
	assert.True(t, res.Success)
	require.Len(t, res.Steps, 3)

	// Verify module/action mapping:
	//   file     → file.manage
	//   copy     → file.copy
	//   template → file.template
	// The Ansible module name maps to a dotted "module.action" reference; the
	// LEVEE Step.Module is the first component ("file" for all three), and
	// Step.Action distinguishes them.
	for _, s := range res.Steps {
		assert.Equal(t, "file", s.Module, "step %q should map to file module", s.Name)
	}
	assert.Equal(t, "manage", res.Steps[0].Action)
	assert.Equal(t, "copy", res.Steps[1].Action)
	assert.Equal(t, "template", res.Steps[2].Action)
}

// TestIntegration_RiskHighPlaybook imports an apt-based playbook and verifies
// that the risk assessor flags it as high risk with a RiskNoRollback finding
// (pkg.install is a write action with no rollback plan).
func TestIntegration_RiskHighPlaybook(t *testing.T) {
	wf, report := importAndAssess(t, aptHighRiskPlaybook)
	require.Len(t, wf.Steps, 1)

	// apt → pkg.install (write, no rollback) → RiskHigh.
	assert.Equal(t, RiskHigh, report.OverallRisk)
	assert.True(t, containsRiskType(report.Findings, RiskNoRollback),
		"expected a no-rollback finding for apt install")

	// Locate the finding and verify its detail.
	for _, f := range report.Findings {
		if f.RiskType == RiskNoRollback {
			assert.Equal(t, "pkg", f.Module)
			assert.Equal(t, "install", f.Action)
			assert.Contains(t, f.Detail, "without a rollback plan")
		}
	}
}

// TestIntegration_MultiTarget executes a 2-step playbook against 3 targets
// and verifies that the result contains 6 (2 × 3) step entries and that
// every target produces an audit trace.
func TestIntegration_MultiTarget(t *testing.T) {
	wf, _ := importAndAssess(t, multiTargetPlaybook)
	require.Len(t, wf.Steps, 2)

	targets := []string{"host-a", "host-b", "host-c"}
	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, targets)
	require.NoError(t, err)

	// Steps count = targets × steps = 3 × 2 = 6.
	require.Len(t, res.Steps, 6)

	// Every step on every target should succeed (shell is supported).
	for _, s := range res.Steps {
		assert.True(t, s.Success, "target=%q step=%q should succeed", s.Target, s.Name)
	}

	// Verify target-major ordering: host-a/s1, host-a/s2, host-b/s1, ...
	assert.Equal(t, "host-a", res.Steps[0].Target)
	assert.Equal(t, "host-a", res.Steps[1].Target)
	assert.Equal(t, "host-b", res.Steps[2].Target)
	assert.Equal(t, "host-b", res.Steps[3].Target)
	assert.Equal(t, "host-c", res.Steps[4].Target)
	assert.Equal(t, "host-c", res.Steps[5].Target)

	// Audit traces: 6 step_execute events (one per target × step).
	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)
	assert.Equal(t, 6, countTraces(traces, audit.EventStepExecute))
}

// TestIntegration_AuditTraceVerification executes a playbook and verifies
// that every step produces a corresponding step_execute audit trace with the
// expected Event field and run id.
func TestIntegration_AuditTraceVerification(t *testing.T) {
	wf, _ := importAndAssess(t, shellOnlyPlaybook)
	require.Len(t, wf.Steps, 2)

	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, []string{"trace-host"})
	require.NoError(t, err)

	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)

	// One step_execute trace per step.
	stepTraces := countTraces(traces, audit.EventStepExecute)
	assert.Equal(t, len(wf.Steps), stepTraces,
		"expected one step_execute trace per step")

	// Every trace should carry the run id and a recognised event type.
	for _, tr := range traces {
		assert.Equal(t, res.RunID, tr.RunID)
		switch tr.Event {
		case audit.EventStepExecute,
			audit.EventGateCheck,
			audit.EventApprovalDecision:
			// expected event types
		default:
			t.Errorf("unexpected trace event %q", tr.Event)
		}
	}
}

// TestIntegration_ApprovalInPlaybook verifies that a step with an ApprovalSpec
// produces an approval_decision audit trace. The Ansible importer does not
// translate approval directives, so we attach the approval manually after
// import to exercise the executor's approval-recording path.
func TestIntegration_ApprovalInPlaybook(t *testing.T) {
	wf, _ := importAndAssess(t, fullPipelinePlaybook)
	require.Len(t, wf.Steps, 3)

	// Attach an approval requirement to the first step.
	wf.Steps[0].Approval = &dsl.ApprovalSpec{
		Level:        "high",
		MinApprovers: 2,
		Approvers:    []string{"alice", "bob"},
	}

	exec := newExecutor(t)
	res, err := exec.Execute(context.Background(), wf, []string{"web-1"})
	require.NoError(t, err)

	// Verify an approval_decision trace was recorded.
	traces, err := exec.recorder.ListByRun(context.Background(), res.RunID)
	require.NoError(t, err)
	assert.Equal(t, 1, countTraces(traces, audit.EventApprovalDecision),
		"expected one approval_decision trace")
}

// TestIntegration_ImportAndAssess verifies the import + assess combination:
// the RiskReport carries the workflow name and a non-empty summary.
func TestIntegration_ImportAndAssess(t *testing.T) {
	wf, report := importAndAssess(t, fullPipelinePlaybook)
	require.NotNil(t, wf)
	require.NotNil(t, report)

	// The Ansible importer does not set wf.Meta.Name, so the assessor falls
	// back to "unnamed".
	assert.Equal(t, "unnamed", report.WorkflowName)
	assert.NotEmpty(t, report.Summary)
	// The summary should mention the workflow name and the overall risk.
	assert.Contains(t, report.Summary, "unnamed")
}
