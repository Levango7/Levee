// Package compat provides the compatibility layer framework that imports
// external automation formats into LEVEE's internal DSL AST.
//
// This file (risk.go) implements the static risk assessor (RiskAssessor)
// that scans an imported dsl.Workflow for high-risk patterns and produces a
// RiskReport. The assessor is a pure static analysis: it never executes
// anything and depends only on internal/dsl plus the standard library,
// satisfying the R8 independence constraint.
//
// Detected risk patterns:
//   - shell.exec non-idempotent commands          (RiskNonIdempotent, medium)
//   - ignore_errors: true directives              (RiskIgnoreErrors, medium)
//   - write operations without a rollback plan    (RiskNoRollback,   high)
//   - inherently irreversible operations          (RiskIrreversible, high)
package compat

import (
	"errors"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/dsl"
)

// --- sentinel errors -------------------------------------------------------

var (
	// ErrEmptyWorkflow is returned when a risk assessment is requested on a
	// nil workflow. Callers may use errors.Is to detect this condition.
	ErrEmptyWorkflow = errors.New("compat: empty workflow")
)

// --- RiskLevel -------------------------------------------------------------

// RiskLevel classifies the severity of a risk finding or the overall risk of
// a workflow. Higher values represent more severe risk.
type RiskLevel int

const (
	// RiskLow indicates low risk: the operation is safe, idempotent and
	// reversible or read-only.
	RiskLow RiskLevel = iota
	// RiskMedium indicates medium risk: the operation may be non-idempotent
	// or tolerates errors, which can mask real problems.
	RiskMedium
	// RiskHigh indicates high risk: the operation is irreversible or
	// performs a write without a rollback plan.
	RiskHigh
)

// String returns a human-readable representation of the risk level.
func (l RiskLevel) String() string {
	switch l {
	case RiskLow:
		return "low"
	case RiskMedium:
		return "medium"
	case RiskHigh:
		return "high"
	default:
		return fmt.Sprintf("unknown(%d)", int(l))
	}
}

// --- RiskType --------------------------------------------------------------

// RiskType identifies the category of a risk finding.
type RiskType int

const (
	// RiskNonIdempotent marks a non-idempotent operation: repeated executions
	// may produce different results (e.g. shell.exec with side effects).
	RiskNonIdempotent RiskType = iota
	// RiskIgnoreErrors marks a step that uses ignore_errors: true, which
	// suppresses failures and can hide real problems.
	RiskIgnoreErrors
	// RiskNoRollback marks a write operation that has no rollback plan,
	// making failures unrecoverable.
	RiskNoRollback
	// RiskIrreversible marks an inherently irreversible operation (e.g.
	// package removal, user deletion, file deletion).
	RiskIrreversible
)

// String returns a human-readable representation of the risk type.
func (t RiskType) String() string {
	switch t {
	case RiskNonIdempotent:
		return "non-idempotent"
	case RiskIgnoreErrors:
		return "ignore-errors"
	case RiskNoRollback:
		return "no-rollback"
	case RiskIrreversible:
		return "irreversible"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// --- RiskFinding -----------------------------------------------------------

// RiskFinding describes a single risk detected in a workflow step.
type RiskFinding struct {
	StepName  string    // Name of the risky step.
	Module    string    // Step module (e.g. "shell", "pkg").
	Action    string    // Step action (e.g. "exec", "install").
	RiskType  RiskType  // Category of the risk.
	RiskLevel RiskLevel // Severity of the risk.
	Detail    string    // Human-readable detail explaining the risk.
}

// --- RiskReport ------------------------------------------------------------

// RiskReport is the result of assessing a workflow's risk. It aggregates all
// findings and an overall risk level with a summary string.
type RiskReport struct {
	WorkflowName string        // Name of the assessed workflow.
	OverallRisk  RiskLevel     // Highest risk level across all findings.
	Findings     []RiskFinding // Individual risk findings, in step order.
	Summary      string        // Human-readable summary of the assessment.
}

// --- risk classification tables --------------------------------------------

// writeActions lists "module.action" combinations that perform writes and
// therefore require a rollback plan. When such a step has a nil Rollback, it
// is flagged with RiskNoRollback at RiskHigh.
//
// The set covers the LEVEE actions reachable via the Ansible compatibility
// layer (file.copy, file.manage, file.template, pkg.install, svc.manage,
// user.manage, user.group) plus common native actions (pkg.upgrade,
// svc.stop, svc.restart, svc.start).
var writeActions = map[string]bool{
	"file.copy":     true,
	"file.manage":   true,
	"file.template": true,
	"pkg.install":   true,
	"pkg.upgrade":   true,
	"svc.manage":    true,
	"svc.stop":      true,
	"svc.start":     true,
	"svc.restart":   true,
	"user.manage":   true,
	"user.group":    true,
}

// irreversibleActions lists "module.action" combinations that are inherently
// irreversible: once executed, the effect cannot be undone even with a
// rollback plan. These are flagged with RiskIrreversible at RiskHigh.
var irreversibleActions = map[string]bool{
	"pkg.remove":  true,
	"user.remove": true,
	"file.delete": true,
	"disk.format": true,
	"db.drop":     true,
}

// --- RiskAssessor ----------------------------------------------------------

// RiskAssessor performs static risk analysis on imported playbooks (or any
// dsl.Workflow). It detects high-risk patterns — non-idempotent shell
// commands, ignored errors, write operations without rollback, and
// irreversible operations — and aggregates them into a RiskReport so users
// can understand the risk profile before execution.
//
// The zero value is ready to use; NewRiskAssessor is provided for symmetry
// with the rest of the package. A RiskAssessor is safe for concurrent use:
// it carries no mutable state.
type RiskAssessor struct{}

// NewRiskAssessor returns a new RiskAssessor ready to evaluate workflows.
func NewRiskAssessor() *RiskAssessor {
	return &RiskAssessor{}
}

// Assess evaluates the risk profile of wf and returns a RiskReport. The
// report's Findings are in step order; OverallRisk is the maximum level of
// any finding (RiskLow if there are none); Summary is a one-line
// human-readable description.
//
// If wf is nil, Assess returns a report with OverallRisk RiskLow and a
// single finding-free summary noting the empty workflow. This keeps Assess
// total (never panics) while still surfacing the condition via the report.
func (a *RiskAssessor) Assess(wf *dsl.Workflow) *RiskReport {
	if wf == nil {
		return &RiskReport{
			WorkflowName: "",
			OverallRisk:  RiskLow,
			Findings:     nil,
			Summary:      "empty workflow: no steps assessed",
		}
	}

	report := &RiskReport{
		WorkflowName: workflowName(wf),
		Findings:     make([]RiskFinding, 0),
	}

	for _, step := range wf.Steps {
		findings := assessStep(step)
		report.Findings = append(report.Findings, findings...)
	}

	report.OverallRisk = computeOverallRisk(report.Findings)
	report.Summary = buildSummary(report)
	return report
}

// workflowName returns the workflow's name from metadata, falling back to
// "unnamed" when the name is empty so the report always has a non-empty
// label.
func workflowName(wf *dsl.Workflow) string {
	if wf.Meta.Name != "" {
		return wf.Meta.Name
	}
	return "unnamed"
}

// assessStep evaluates a single step and returns all risk findings that
// apply. A step may produce multiple findings (e.g. a non-idempotent shell
// command that also ignores errors).
func assessStep(step dsl.Step) []RiskFinding {
	var findings []RiskFinding

	dotted := step.Module + "." + step.Action
	stepLabel := step.Name
	if stepLabel == "" {
		stepLabel = dotted
	}

	// 1. Non-idempotent: shell.exec commands typically have side effects
	//    and are not safe to re-run.
	if dotted == "shell.exec" {
		findings = append(findings, RiskFinding{
			StepName:  step.Name,
			Module:    step.Module,
			Action:    step.Action,
			RiskType:  RiskNonIdempotent,
			RiskLevel: RiskMedium,
			Detail:    fmt.Sprintf("shell command %q is non-idempotent: repeated runs may produce different results", shellCommand(step)),
		})
	}

	// 2. ignore_errors: true suppresses failures and can hide real problems.
	if isIgnoreErrors(step) {
		findings = append(findings, RiskFinding{
			StepName:  step.Name,
			Module:    step.Module,
			Action:    step.Action,
			RiskType:  RiskIgnoreErrors,
			RiskLevel: RiskMedium,
			Detail:    fmt.Sprintf("step %q sets ignore_errors: true; failures will be suppressed and may mask real problems", stepLabel),
		})
	}

	// 3. Irreversible operations are always high risk, regardless of
	//    rollback presence.
	if irreversibleActions[dotted] {
		findings = append(findings, RiskFinding{
			StepName:  step.Name,
			Module:    step.Module,
			Action:    step.Action,
			RiskType:  RiskIrreversible,
			RiskLevel: RiskHigh,
			Detail:    fmt.Sprintf("step %q performs irreversible action %s.%s; the effect cannot be undone", stepLabel, step.Module, step.Action),
		})
	} else if step.Rollback == nil && writeActions[dotted] {
		// 4. Write operation without a rollback plan: high risk because a
		//    failure leaves the system in a changed state with no recovery.
		findings = append(findings, RiskFinding{
			StepName:  step.Name,
			Module:    step.Module,
			Action:    step.Action,
			RiskType:  RiskNoRollback,
			RiskLevel: RiskHigh,
			Detail:    fmt.Sprintf("step %q performs write action %s.%s without a rollback plan", stepLabel, step.Module, step.Action),
		})
	}

	return findings
}

// isIgnoreErrors reports whether the step's Args set ignore_errors to true.
// The value may be a bool or a string "true"; both forms are accepted to
// accommodate YAML parsing differences.
func isIgnoreErrors(step dsl.Step) bool {
	v, ok := step.Args["ignore_errors"]
	if !ok {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(val, "true")
	default:
		return false
	}
}

// shellCommand returns a short representation of the shell command stored in
// step.Args for use in finding details. It prefers the "cmd" key and falls
// back to a generic placeholder.
func shellCommand(step dsl.Step) string {
	if cmd, ok := step.Args["cmd"]; ok {
		if s, ok := cmd.(string); ok && s != "" {
			return s
		}
	}
	return "<inline>"
}

// computeOverallRisk returns the highest RiskLevel among the findings, or
// RiskLow when there are none.
func computeOverallRisk(findings []RiskFinding) RiskLevel {
	overall := RiskLow
	for _, f := range findings {
		if f.RiskLevel > overall {
			overall = f.RiskLevel
		}
	}
	return overall
}

// buildSummary produces a one-line human-readable summary of the report.
func buildSummary(r *RiskReport) string {
	if len(r.Findings) == 0 {
		return fmt.Sprintf("workflow %q: no risks detected (risk: %s)", r.WorkflowName, r.OverallRisk)
	}

	high, medium, low := 0, 0, 0
	for _, f := range r.Findings {
		switch f.RiskLevel {
		case RiskHigh:
			high++
		case RiskMedium:
			medium++
		case RiskLow:
			low++
		}
	}

	var parts []string
	if high > 0 {
		parts = append(parts, fmt.Sprintf("%d high", high))
	}
	if medium > 0 {
		parts = append(parts, fmt.Sprintf("%d medium", medium))
	}
	if low > 0 {
		parts = append(parts, fmt.Sprintf("%d low", low))
	}

	return fmt.Sprintf("workflow %q: %d finding(s) (%s) — overall risk: %s",
		r.WorkflowName, len(r.Findings), strings.Join(parts, ", "), r.OverallRisk)
}
