// Package plan dry-run preview. The DryRunPreview simulates plan
// generation and execution planning without performing any real
// action. It produces a DryRunReport containing the resolved target
// set, batch division, blast-radius impact, estimated duration and
// potential conflicts so that operators can review the change scope
// before applying it.
//
// DryRunPreview holds no state.Store reference: it is a pure read-only
// previewer and never persists anything. Multiple Preview calls on the
// same workflow produce equivalent reports (modulo timestamps embedded
// in the underlying Plan, which are not exposed in the report).
package plan

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nexus/levee/internal/dsl"
)

// Sentinel errors returned by the dry-run previewer.
var (
	// ErrDryRunFailed is returned when the dry-run preview cannot be
	// produced, e.g. nil workflow, no targets or cancelled context.
	ErrDryRunFailed = errors.New("plan: dry-run failed")
)

// Conflict type constants. Kept as strings to make the report directly
// JSON-serializable and to match the spec wording.
const (
	// ConflictTypeLockConflict indicates a target appears in more than
	// one batch, which would require re-locking an already-locked host.
	ConflictTypeLockConflict = "lock_conflict"

	// ConflictTypeConcurrentEdit indicates a batch contains multiple
	// write actions that may edit the same resource concurrently.
	ConflictTypeConcurrentEdit = "concurrent_edit"

	// ConflictTypeResourceContention indicates a batch runs with
	// unlimited concurrency (MaxConcurrency=0) over a large target set,
	// which may overwhelm shared resources.
	ConflictTypeResourceContention = "resource_contention"
)

// Default step durations keyed by "module.action". Unknown step types
// fall back to DefaultStepDuration. The values are conservative
// estimates used solely for preview timing; real execution may differ.
const (
	// DefaultStepDuration is the fallback duration for unknown step
	// types.
	DefaultStepDuration = 5 * time.Second

	// ResourceContentionThreshold is the minimum target count in an
	// unlimited-concurrency batch above which a resource_contention
	// conflict is reported.
	ResourceContentionThreshold = 10
)

// defaultStepDurations maps a "module.action" key to its estimated
// duration. Lookup is case-sensitive. Unknown keys use
// DefaultStepDuration.
var defaultStepDurations = map[string]time.Duration{
	"shell.exec":    5 * time.Second,
	"file.copy":     2 * time.Second,
	"file.template": 2 * time.Second,
	"pkg.install":   30 * time.Second,
	"svc.manage":    5 * time.Second,
	"user.manage":   3 * time.Second,
}

// writeActions is the set of "module.action" keys that perform writes.
// A batch containing more than one write action is flagged with a
// concurrent_edit conflict because the actions may edit the same
// resource.
var writeActions = map[string]bool{
	"file.copy":     true,
	"file.template": true,
	"pkg.install":   true,
	"pkg.remove":    true,
	"svc.restart":   true,
	"svc.stop":      true,
	"user.create":   true,
	"user.remove":   true,
}

// DryRunPreview generates a dry-run preview report. It simulates plan
// generation and execution planning but never performs any real action.
// The report contains the target set, batch division, blast-radius
// impact, estimated duration and potential conflicts, helping operators
// understand the change scope before applying it.
//
// DryRunPreview is stateless and safe for concurrent use. It holds no
// state.Store reference: Preview is a pure read-only operation.
type DryRunPreview struct {
	// generator produces the underlying Plan used for preview.
	generator *Generator

	// impact analyzes the blast radius of the generated Plan.
	impact *ImpactAnalyzer
}

// NewDryRunPreview returns a ready-to-use DryRunPreview backed by the
// given Generator and ImpactAnalyzer. Both must be non-nil; a nil
// argument yields a previewer that returns ErrDryRunFailed on every
// call.
func NewDryRunPreview(generator *Generator, impact *ImpactAnalyzer) *DryRunPreview {
	return &DryRunPreview{
		generator: generator,
		impact:    impact,
	}
}

// DryRunReport is the preview report produced by DryRunPreview.Preview.
// It aggregates everything an operator needs to review a change before
// applying it: the resolved targets, batch division, blast-radius
// impact, estimated total duration, potential conflicts and warnings.
type DryRunReport struct {
	// WorkflowName is the name of the source workflow.
	WorkflowName string

	// Targets is the full list of resolved target hosts, in the order
	// they appear in the workflow's target groups.
	Targets []string

	// Batches is the ordered list of batch previews. Batches run
	// sequentially; targets within a batch run concurrently.
	Batches []DryRunBatch

	// Impact summarizes the blast radius of the plan.
	Impact ImpactSummary

	// EstDuration is the estimated total duration across all batches.
	// Batches run sequentially so their durations are summed; targets
	// within a batch run concurrently so the batch duration is the
	// maximum single-target duration.
	EstDuration time.Duration

	// Conflicts is the list of potential conflicts detected by the
	// previewer. Sorted by (Type, Target) for stable output.
	Conflicts []PotentialConflict

	// Warnings is the list of advisory warnings (e.g. missing rollback,
	// unlimited concurrency). Not fatal, but worth operator attention.
	Warnings []string
}

// DryRunBatch is a single batch preview within a DryRunReport.
type DryRunBatch struct {
	// Index is the 0-based batch ordinal.
	Index int

	// Targets is the list of target hosts in this batch.
	Targets []string

	// Steps is the list of step names executed on each target in this
	// batch.
	Steps []string

	// EstDuration is the estimated duration of this batch. Targets
	// within a batch run concurrently, so this is the maximum
	// single-target duration (which equals the sum of step durations
	// since every target runs the same step sequence).
	EstDuration time.Duration
}

// ImpactSummary is the blast-radius summary in a DryRunReport. Targets
// are classified into risk tiers based on the overall ImpactReport risk
// level: when the plan risk is high all direct targets are high-risk,
// medium → medium-risk, low → low-risk. Indirect targets are always
// classified as medium-risk because they are affected but not directly
// changed.
type ImpactSummary struct {
	// TotalTargets is the total number of distinct targets affected,
	// directly or indirectly.
	TotalTargets int

	// HighRisk is the list of high-risk targets.
	HighRisk []string

	// MediumRisk is the list of medium-risk targets.
	MediumRisk []string

	// LowRisk is the list of low-risk targets.
	LowRisk []string
}

// PotentialConflict describes a potential conflict detected during
// dry-run preview.
type PotentialConflict struct {
	// Type is the conflict type: ConflictTypeLockConflict,
	// ConflictTypeConcurrentEdit or ConflictTypeResourceContention.
	Type string

	// Target is the target or batch identifier involved in the
	// conflict.
	Target string

	// Detail is a human-readable description of the conflict.
	Detail string
}

// Preview generates a dry-run preview report for the given workflow.
// It calls the Generator to build a Plan, the ImpactAnalyzer to compute
// the blast radius, then derives the estimated duration, potential
// conflicts and warnings. No real action is performed and nothing is
// persisted.
//
// Returns ErrDryRunFailed (wrapped) when:
//   - wf is nil;
//   - the context is cancelled;
//   - the workflow has no resolvable static targets;
//   - the Generator rejects the workflow.
func (p *DryRunPreview) Preview(ctx context.Context, wf *dsl.Workflow) (*DryRunReport, error) {
	if p == nil || p.generator == nil || p.impact == nil {
		return nil, fmt.Errorf("%w: previewer not initialized", ErrDryRunFailed)
	}
	if wf == nil {
		return nil, fmt.Errorf("%w: workflow is nil", ErrDryRunFailed)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDryRunFailed, err)
	}

	// Extract static targets from the workflow's target groups.
	targets := extractStaticTargets(wf)
	if len(targets) == 0 {
		return nil, fmt.Errorf("%w: no resolvable static targets", ErrDryRunFailed)
	}

	// Generate the plan (simulated, not applied).
	plan, err := p.generator.Generate(wf, targets)
	if err != nil {
		return nil, fmt.Errorf("%w: generate plan: %v", ErrDryRunFailed, err)
	}

	// Analyze the blast radius.
	impactReport := p.impact.Analyze(plan)

	// Assemble the report. No field here triggers any side effect.
	report := &DryRunReport{
		WorkflowName: plan.WorkflowName,
		Targets:      targets,
		Batches:      p.buildBatches(plan),
		Impact:       p.buildImpactSummary(impactReport),
		EstDuration:  p.estimateDuration(plan),
		Conflicts:    p.detectConflicts(plan),
		Warnings:     p.generateWarnings(wf, plan),
	}
	return report, nil
}

// extractStaticTargets collects all static hosts from the workflow's
// target groups. Dynamic groups (Query populated, Hosts empty) are
// skipped because dry-run cannot resolve them without an external
// inventory. The order follows the workflow's target group declaration
// order, preserving operator intent.
func extractStaticTargets(wf *dsl.Workflow) []string {
	var targets []string
	for _, g := range wf.Targets {
		targets = append(targets, g.Hosts...)
	}
	return targets
}

// buildBatches converts a Plan's batches into DryRunBatch previews,
// extracting step names and per-batch estimated durations.
func (p *DryRunPreview) buildBatches(plan *Plan) []DryRunBatch {
	out := make([]DryRunBatch, len(plan.Batches))
	for i, b := range plan.Batches {
		stepNames := make([]string, len(b.Steps))
		for j, s := range b.Steps {
			stepNames[j] = s.Name
		}
		out[i] = DryRunBatch{
			Index:       b.Index,
			Targets:     b.Targets,
			Steps:       stepNames,
			EstDuration: estimateBatchDuration(b),
		}
	}
	return out
}

// buildImpactSummary converts an ImpactReport into an ImpactSummary,
// classifying targets into risk tiers. Direct targets inherit the
// overall plan risk level; indirect targets are always medium-risk.
func (p *DryRunPreview) buildImpactSummary(report *ImpactReport) ImpactSummary {
	if report == nil {
		return ImpactSummary{}
	}

	summary := ImpactSummary{
		TotalTargets: report.TotalAffected,
	}

	// Classify direct targets by the overall plan risk level.
	switch report.RiskLevel {
	case RiskLevelHigh:
		summary.HighRisk = append(summary.HighRisk, report.DirectTargets...)
	case RiskLevelMedium:
		summary.MediumRisk = append(summary.MediumRisk, report.DirectTargets...)
	default:
		summary.LowRisk = append(summary.LowRisk, report.DirectTargets...)
	}

	// Indirect targets are always medium-risk: they are affected but
	// not directly changed, so the risk is moderate.
	if len(report.IndirectTargets) > 0 {
		summary.MediumRisk = append(summary.MediumRisk, report.IndirectTargets...)
		sort.Strings(summary.MediumRisk)
	}

	return summary
}

// estimateDuration computes the estimated total duration across all
// batches. Batches run sequentially so their durations are summed.
func (p *DryRunPreview) estimateDuration(plan *Plan) time.Duration {
	var total time.Duration
	for _, b := range plan.Batches {
		total += estimateBatchDuration(b)
	}
	return total
}

// estimateBatchDuration estimates the duration of a single batch.
// Every target in the batch runs the same step sequence concurrently,
// so the batch duration is the maximum single-target duration. Since
// all targets share the same steps, the maximum equals the sum of step
// durations. A batch with no targets has zero duration.
func estimateBatchDuration(b Batch) time.Duration {
	if len(b.Targets) == 0 {
		return 0
	}
	// Single-target duration = sum of step durations (steps run
	// sequentially on each target).
	var singleTarget time.Duration
	for _, s := range b.Steps {
		singleTarget += stepDuration(s)
	}
	// In-batch concurrency: take the maximum across targets. All
	// targets run the same steps, so max == singleTarget.
	return singleTarget
}

// stepDuration returns the estimated duration of a single step based on
// its "module.action" key. Unknown step types fall back to
// DefaultStepDuration.
func stepDuration(s PlanStep) time.Duration {
	key := s.Module + "." + s.Action
	if d, ok := defaultStepDurations[key]; ok {
		return d
	}
	return DefaultStepDuration
}

// detectConflicts performs static conflict analysis on the plan. It
// detects three conflict kinds:
//   - lock_conflict: a target appears in more than one batch;
//   - concurrent_edit: a batch contains multiple write actions;
//   - resource_contention: a batch runs with unlimited concurrency over
//     a large target set.
//
// The returned slice is sorted by (Type, Target) for stable output.
func (p *DryRunPreview) detectConflicts(plan *Plan) []PotentialConflict {
	var conflicts []PotentialConflict

	// 1. lock_conflict: a target appearing in multiple batches would
	// require re-locking an already-locked host.
	targetBatches := make(map[string][]int)
	for _, b := range plan.Batches {
		for _, t := range b.Targets {
			targetBatches[t] = append(targetBatches[t], b.Index)
		}
	}
	for target, batches := range targetBatches {
		if len(batches) > 1 {
			conflicts = append(conflicts, PotentialConflict{
				Type:   ConflictTypeLockConflict,
				Target: target,
				Detail: fmt.Sprintf("target appears in %d batches: %v", len(batches), batches),
			})
		}
	}

	// 2. concurrent_edit: multiple write actions in one batch may edit
	// the same resource.
	for _, b := range plan.Batches {
		writeCount := 0
		for _, s := range b.Steps {
			if writeActions[s.Module+"."+s.Action] {
				writeCount++
			}
		}
		if writeCount > 1 {
			conflicts = append(conflicts, PotentialConflict{
				Type:   ConflictTypeConcurrentEdit,
				Target: fmt.Sprintf("batch-%d", b.Index),
				Detail: fmt.Sprintf("batch has %d write steps that may edit the same resource", writeCount),
			})
		}
	}

	// 3. resource_contention: unlimited concurrency over a large batch
	// may overwhelm shared resources.
	for _, b := range plan.Batches {
		if b.MaxConcurrency == 0 && len(b.Targets) > ResourceContentionThreshold {
			conflicts = append(conflicts, PotentialConflict{
				Type:   ConflictTypeResourceContention,
				Target: fmt.Sprintf("batch-%d", b.Index),
				Detail: fmt.Sprintf("batch has %d targets with unlimited concurrency (MaxConcurrency=0)", len(b.Targets)),
			})
		}
	}

	// Sort by (Type, Target) for stable output.
	sort.Slice(conflicts, func(i, j int) bool {
		if conflicts[i].Type != conflicts[j].Type {
			return conflicts[i].Type < conflicts[j].Type
		}
		return conflicts[i].Target < conflicts[j].Target
	})

	return conflicts
}

// generateWarnings produces advisory warnings for the operator. Warnings
// are not fatal but worth reviewing before applying the plan:
//   - workflow has no rollback plan;
//   - a batch runs with unlimited concurrency and more than one target.
func (p *DryRunPreview) generateWarnings(wf *dsl.Workflow, plan *Plan) []string {
	var warnings []string

	if wf.Rollback == nil {
		warnings = append(warnings, "workflow has no rollback plan")
	}

	for _, b := range plan.Batches {
		if b.MaxConcurrency == 0 && len(b.Targets) > 1 {
			warnings = append(warnings, fmt.Sprintf("batch %d has unlimited concurrency with %d targets", b.Index, len(b.Targets)))
		}
	}

	return warnings
}
