package autoplanner
// planner.go implements the AutoPlanner that converts a
// recommend.Recommendation into a formal Workflow ready for the apply phase.
// The planner parses the recommendation's LEVEELang YAML draft, divides the
// steps into batches, estimates the total execution time and stamps the
// workflow with the risk and approval tier derived from the recommendation.
//
// The AutoPlanner is intentionally pure: it does not execute the workflow
// nor does it call the LLM. It is safe for concurrent use because it carries
// no mutable state.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilRecommendation is returned when Plan is called with a nil
	// recommendation.
	ErrNilRecommendation = errors.New("autoplanner: nil recommendation")
	// ErrEmptyWorkflowDraft is returned when the recommendation has no
	// LEVEELang YAML draft to parse.
	ErrEmptyWorkflowDraft = errors.New("autoplanner: empty workflow draft")
	// ErrInvalidRiskLevel is returned when the recommendation carries a
	// risk level that is not one of the four known values.
	ErrInvalidRiskLevel = errors.New("autoplanner: invalid risk level")
)

// --- Defaults ---------------------------------------------------------------

const (
	// defaultStepDuration is the per-step wall-clock cost used by
	// estimateTime when no step-level estimate is available.
	defaultStepDuration = 30 * time.Second
)

// --- Step -------------------------------------------------------------------

// Step is a single workflow step parsed from the recommendation's
// LEVEELang YAML draft. It is a self-contained value: the OnTarget field
// carries the target host so that the planner can divide steps across
// batches without needing the outer workflow context.
type Step struct {
	// Name is the human-readable step name.
	Name string
	// Module is the LEVEELang module name (shell / svc / pkg / file ...).
	Module string
	// Action is the action verb (restart / install / copy ...).
	Action string
	// Args is the step arguments as key-value pairs.
	Args map[string]string
	// OnTarget is the target host the step runs on. Empty means "all
	// targets in the batch".
	OnTarget string
}

// --- Batch ------------------------------------------------------------------

// Batch is a single execution batch. Targets within a batch run in parallel
// when Parallel is true; batches themselves run sequentially.
type Batch struct {
	// ID is the 1-based batch ordinal.
	ID int
	// Targets is the list of target hosts in this batch.
	Targets []string
	// Steps is the ordered list of steps to execute on each target.
	Steps []Step
	// Parallel reports whether the steps within the batch may run in
	// parallel.
	Parallel bool
}

// --- Workflow ---------------------------------------------------------------

// Workflow is the formal, executable workflow produced by AutoPlanner.Plan.
// It carries the formalised LEVEELang YAML, the batch division, the risk and
// approval tiers, the estimated execution time and the operational metadata
// (pre-conditions, rollback plan, target) copied from the recommendation.
type Workflow struct {
	// ID is a UUID identifying the workflow instance.
	ID string
	// Name is a human-readable name derived from the recommendation.
	Name string
	// YAML is the formalised LEVEELang workflow YAML.
	YAML string
	// Batches is the ordered list of execution batches.
	Batches []Batch
	// RiskLevel is the risk tier copied from the recommendation.
	RiskLevel recommend.RiskLevel
	// ApprovalLevel is the approval tier: standard / high / emergency.
	ApprovalLevel string
	// EstimatedTime is the estimated total wall-clock execution time.
	EstimatedTime time.Duration
	// PreConditions is the list of pre-condition checks copied from the
	// recommendation.
	PreConditions []string
	// RollbackPlan is the rollback plan copied from the recommendation.
	RollbackPlan string
	// Target is the target host / service copied from the recommendation.
	Target string
	// CreatedAt is the workflow creation timestamp (UTC).
	CreatedAt time.Time
}

// --- AutoPlannerConfig ------------------------------------------------------

// AutoPlannerConfig configures an AutoPlanner. Nil fields are replaced with
// sensible defaults by NewAutoPlanner.
type AutoPlannerConfig struct {
	// PlanGen is the plan generator. Nil -> plan.NewGenerator().
	PlanGen *plan.Generator
	// ImpactAna is the impact analyzer. Nil -> plan.NewImpactAnalyzer().
	ImpactAna *plan.ImpactAnalyzer
	// RiskAssess is the risk assessor. Nil -> NewRiskAssessor().
	RiskAssess *RiskAssessor
	// Logger is the structured logger. Nil -> log.With("component", ...).
	Logger *slog.Logger
}

// --- AutoPlanner ------------------------------------------------------------

// AutoPlanner converts a recommend.Recommendation into a formal Workflow.
// It is safe for concurrent use: all fields are read-only after construction.
type AutoPlanner struct {
	planGen    *plan.Generator
	impactAna  *plan.ImpactAnalyzer
	riskAssess *RiskAssessor
	log        *slog.Logger
}

// NewAutoPlanner returns an AutoPlanner with the given config. Nil fields in
// cfg are replaced with sensible defaults so that a zero-value config
// produces a fully wired planner.
func NewAutoPlanner(cfg AutoPlannerConfig) *AutoPlanner {
	planGen := cfg.PlanGen
	if planGen == nil {
		planGen = plan.NewGenerator()
	}
	impactAna := cfg.ImpactAna
	if impactAna == nil {
		impactAna = plan.NewImpactAnalyzer()
	}
	riskAssess := cfg.RiskAssess
	if riskAssess == nil {
		riskAssess = NewRiskAssessor()
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "autoplanner")
	}
	return &AutoPlanner{
		planGen:    planGen,
		impactAna:  impactAna,
		riskAssess: riskAssess,
		log:        lg,
	}
}

// Plan converts a recommendation into a formal Workflow. The conversion
// proceeds in five steps:
//
//  1. Validate the recommendation (non-nil, non-empty draft, known risk).
//  2. Assess the risk -> approval tier.
//  3. Parse the YAML draft into structured steps.
//  4. Divide the steps into batches based on the target.
//  5. Estimate the total execution time and assemble the Workflow.
//
// Plan returns ErrNilRecommendation for a nil recommendation,
// ErrEmptyWorkflowDraft for a recommendation with no YAML draft, and
// ErrInvalidRiskLevel for an unknown risk level.
func (p *AutoPlanner) Plan(ctx context.Context, rec *recommend.Recommendation) (*Workflow, error) {
	if rec == nil {
		return nil, ErrNilRecommendation
	}
	if strings.TrimSpace(rec.WorkflowDraft) == "" {
		return nil, ErrEmptyWorkflowDraft
	}
	if !isValidRiskLevel(rec.RiskLevel) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidRiskLevel, rec.RiskLevel)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	assessment := p.riskAssess.Assess(rec)
	steps := p.parseSteps(rec.WorkflowDraft)
	batches := p.divideBatches(steps, rec.Target)
	estTime := p.estimateTime(steps)
	yaml := p.formaliseYAML(rec, assessment, estTime)

	wf := &Workflow{
		ID:            uuid.NewString(),
		Name:          workflowName(rec),
		YAML:          yaml,
		Batches:       batches,
		RiskLevel:     rec.RiskLevel,
		ApprovalLevel: assessment.ApprovalLevel,
		EstimatedTime: estTime,
		PreConditions: rec.PreConditions,
		RollbackPlan:  rec.RollbackPlan,
		Target:        rec.Target,
		CreatedAt:     time.Now().UTC(),
	}

	p.log.InfoContext(ctx, "autoplanner: workflow planned",
		"workflow_id", wf.ID,
		"target", wf.Target,
		"risk_level", string(wf.RiskLevel),
		"approval_level", wf.ApprovalLevel,
		"batches", len(wf.Batches),
		"steps", len(steps),
		"estimated_time", wf.EstimatedTime.String(),
	)

	return wf, nil
}

// parseSteps extracts the step list from a LEVEELang YAML draft. The parser
// is intentionally lightweight: it scans for "steps:" blocks and reads the
// name / module / action / target fields of each step. When the YAML does not
// contain a steps block, parseSteps returns a single synthetic step that
// wraps the raw draft so the operator can still inspect it.
func (p *AutoPlanner) parseSteps(yaml string) []Step {
	lines := strings.Split(yaml, "\n")
	var steps []Step
	inSteps := false
	var cur Step
	pending := false

	flush := func() {
		if pending {
			if cur.Name == "" {
				cur.Name = fmt.Sprintf("step-%d", len(steps)+1)
			}
			if cur.Module == "" {
				cur.Module = "shell"
			}
			if cur.Action == "" {
				cur.Action = "run"
			}
			steps = append(steps, cur)
			cur = Step{}
			pending = false
		}
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		// Skip blank lines and comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Detect the start of a steps block. The key may appear as
		// "steps:" or "- steps:" (when nested under a batch list item).
		if strings.HasPrefix(trimmed, "steps:") || strings.HasPrefix(trimmed, "- steps:") {
			inSteps = true
			continue
		}

		if !inSteps {
			continue
		}

		// "rollback:" always terminates the steps block, even mid-step,
		// because rollback entries share the "- name:" list marker with
		// steps and would otherwise be mis-parsed as steps.
		if strings.HasPrefix(trimmed, "rollback:") || strings.HasPrefix(trimmed, "- rollback:") {
			flush()
			inSteps = false
			continue
		}

		// Other sibling / top-level keys terminate the steps block only
		// when we are not in the middle of a step entry. This avoids
		// consuming unrelated list items after the steps section.
		if !pending && isBlockTerminator(trimmed) {
			inSteps = false
			continue
		}

		// A new step entry begins with "- name:" (YAML list item).
		if strings.HasPrefix(trimmed, "- name:") {
			flush()
			cur.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "- name:"))
			pending = true
			continue
		}

		if !pending {
			// A bare "name:" at the step indent starts the first step
			// when no list marker is present.
			if strings.HasPrefix(trimmed, "name:") {
				cur.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
				pending = true
			}
			continue
		}

		// Inside a step: read module / action / target / name fields.
		switch {
		case strings.HasPrefix(trimmed, "module:"):
			cur.Module = strings.TrimSpace(strings.TrimPrefix(trimmed, "module:"))
		case strings.HasPrefix(trimmed, "action:"):
			cur.Action = strings.TrimSpace(strings.TrimPrefix(trimmed, "action:"))
		case strings.HasPrefix(trimmed, "target:"):
			cur.OnTarget = strings.TrimSpace(strings.TrimPrefix(trimmed, "target:"))
		case strings.HasPrefix(trimmed, "name:"):
			cur.Name = strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
		}
	}
	flush()

	if len(steps) == 0 {
		// No steps block found: wrap the raw draft in a single review
		// step so the operator can still inspect it.
		steps = []Step{{
			Name:   "review-draft",
			Module: "shell",
			Action: "run",
			Args:   map[string]string{"draft": yaml},
		}}
	}
	return steps
}

// divideBatches partitions the step list into batches based on the target
// topology. A single target yields a single batch; multiple targets (comma
// or space separated) yield one batch per target so that the apply phase
// can roll forward host by host. All steps are replicated into every batch.
func (p *AutoPlanner) divideBatches(steps []Step, target string) []Batch {
	targets := splitTargets(target)
	if len(targets) == 0 {
		targets = []string{"all"}
	}

	batches := make([]Batch, len(targets))
	for i, t := range targets {
		batches[i] = Batch{
			ID:       i + 1,
			Targets:  []string{t},
			Steps:    steps,
			Parallel: len(steps) > 1,
		}
	}
	return batches
}

// estimateTime sums the per-step duration of the step list. Each step is
// assumed to take defaultStepDuration; the total is len(steps) *
// defaultStepDuration. A zero-step workflow returns zero.
func (p *AutoPlanner) estimateTime(steps []Step) time.Duration {
	if len(steps) == 0 {
		return 0
	}
	return time.Duration(len(steps)) * defaultStepDuration
}

// formaliseYAML produces the formal LEVEELang YAML for the workflow. It
// preserves the recommendation's draft and appends a comment block carrying
// the resolved approval tier, risk level and estimated time so that the
// apply phase receives a self-contained document.
func (p *AutoPlanner) formaliseYAML(rec *recommend.Recommendation, assessment Assessment, est time.Duration) string {
	var b strings.Builder
	b.WriteString(rec.WorkflowDraft)
	if !strings.HasSuffix(rec.WorkflowDraft, "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "# --- autoplanner formalisation ---\n")
	fmt.Fprintf(&b, "# approval: %s\n", assessment.ApprovalLevel)
	fmt.Fprintf(&b, "# risk: %s\n", string(rec.RiskLevel))
	fmt.Fprintf(&b, "# estimated_time: %s\n", est.String())
	return b.String()
}

// workflowName derives a human-readable workflow name from the recommendation.
// It prefers the summary, falls back to "auto-fix-<target>" and finally to
// "auto-fix-unnamed" when neither is available.
func workflowName(rec *recommend.Recommendation) string {
	if rec.Summary != "" {
		return rec.Summary
	}
	if rec.Target != "" {
		return "auto-fix-" + rec.Target
	}
	return "auto-fix-unnamed"
}

// isValidRiskLevel reports whether level is one of the four known risk
// levels defined in the recommend package.
func isValidRiskLevel(level recommend.RiskLevel) bool {
	switch level {
	case recommend.RiskLow, recommend.RiskMedium, recommend.RiskHigh, recommend.RiskCritical:
		return true
	default:
		return false
	}
}

// isBlockTerminator reports whether a trimmed YAML line introduces a sibling
// or top-level block that ends the current steps section. The recognised
// terminators are the top-level LEVEELang keys that may follow a steps block.
// "name:" and "- name:" are deliberately excluded because they are step
// entry markers, not block terminators.
func isBlockTerminator(trimmed string) bool {
	terminators := []string{
		"target:", "- target:",
		"window:", "- window:",
		"batches:", "- batches:",
		"description:", "- description:",
		"approval:", "- approval:",
	}
	for _, t := range terminators {
		if strings.HasPrefix(trimmed, t) {
			return true
		}
	}
	return false
}

// splitTargets splits a target string into individual target hosts. Targets
// may be separated by commas, spaces or both. Empty tokens are dropped and
// duplicates are removed while preserving the first-seen order.
func splitTargets(target string) []string {
	if target == "" {
		return nil
	}
	// Normalise commas to spaces, then split on whitespace.
	normalised := strings.ReplaceAll(target, ",", " ")
	fields := strings.Fields(normalised)
	if len(fields) == 0 {
		return nil
	}
	// Deduplicate while preserving order.
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	return out
}