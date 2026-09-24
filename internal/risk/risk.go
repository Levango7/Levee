// Package risk scores a plan's danger and derives the minimum approval
// tier it must clear before execution (design red-line R4: 不可逆变更必须
// 更高级别审批). The score is computed at plan time from the plan
// artifact alone — the same single source of truth the approval tier
// routing and the rollback gating read — so what was scored is exactly
// what was approved and executed.
//
// The package deliberately does NOT import internal/plan: the plan
// package carries the risk fields on its artifact types (so they
// serialise into plan_json and join the plan hash), and importing plan
// here would create a cycle. Instead the Assessor consumes a flattened
// Input the caller assembles from a plan; the blast-radius factor takes
// the direct/indirect target counts the caller computed with the plan
// package's own ImpactAnalyzer.
//
// Factors (each contributes a documented number of points):
//
//   - irreversible steps: each irreversible step adds weight (the
//     verdict comes from the executor's IrreversibleChecker as stamped
//     by the plan generator — batch 1 wiring);
//   - destructive action names: action names carrying remove / delete /
//     drop / truncate / destroy / purge raise the score even when the
//     author did not declare the step irreversible (defence in depth:
//     the whitelist can lag a module);
//   - blast radius: direct plus indirect targets, banded low/medium/high
//     (the caller maps the ImpactAnalyzer's tier onto the bands);
//   - rollback coverage: steps without any rollback declaration reduce
//     the change's reversibility even when every individual action is
//     reversible (R2: 回滚可行性必须显式);
//   - batch fan-out: a plan spread over many batches carries more
//     sequencing risk than a single-batch one.
//
// The output is deliberately explainable: Assessment.Factors carries one
// entry per contributing rule so the CLI / report can show WHY a change
// was routed to high or emergency review. Scores map to an ApprovalFloor
// (standard / high / emergency) that the approval routing combines with
// the workflow's own declaration as max(declared, floor) — never lower
// than either.

package risk

import (
	"fmt"
	"sort"
	"strings"
)

// Approval tier names, matching the LEVEELang spec's three legal levels.
const (
	LevelStandard  = "standard"
	LevelHigh      = "high"
	LevelEmergency = "emergency"
)

// Score ceilings per factor class. The numbers are policy, not physics:
// they are tuned so that a single irreversible step alone reaches the
// high floor (the R4 red line) and only the conjunction of several
// danger signals escalates to emergency.
const (
	// pointsIrreversibleStep is added per irreversible step (capped).
	pointsIrreversibleStep = 20
	// maxIrreversiblePoints caps the irreversible-step contribution so a
	// 30-step plan with 25 irreversible steps does not drown the other
	// signals.
	maxIrreversiblePoints = 40

	// pointsDestructiveAction is added per step whose action name is on
	// the destructive lexicon (capped like above).
	pointsDestructiveAction = 8
	maxDestructivePoints    = 24

	// Blast radius bands (aligned with the ImpactAnalyzer tiers):
	// low (<10 affected) adds nothing; medium (10-50) one band;
	// high (>50) two bands.
	pointsPerBlastBand = 10

	// pointsNoRollbackStep is added per step lacking any rollback
	// declaration (uncovered reversibility), capped.
	pointsNoRollbackStep = 5
	maxNoRollbackPoints  = 15

	// pointsPerBatch adds sequencing risk per batch beyond the first.
	pointsPerBatch = 2
	maxBatchPoints = 6
)

// Step is one step's risk-relevant projection. The caller flattens a
// plan step into this shape (name, action, irreversibility verdict,
// rollback presence); nothing else about the step matters to scoring.
type Step struct {
	Name         string
	Action       string
	Irreversible bool
	HasRollback  bool
}

// Input is the flattened plan description the Assessor scores. The
// caller assembles it from a *plan.Plan without this package importing
// plan (cycle avoidance, see the package comment).
type Input struct {
	// Steps lists every step across all batches (order irrelevant).
	Steps []Step

	// BatchCount is the number of sequential batches.
	BatchCount int

	// DirectTargets is the count of distinct directly-changed hosts.
	DirectTargets int

	// IndirectTargets is the count of distinct indirectly-affected
	// hosts (not already counted in DirectTargets).
	IndirectTargets int

	// BlastHigh reports whether the blast radius falls in the
	// ImpactAnalyzer's HIGH band (> 50 affected). The emergency
	// escalation requires irreversible AND wide blast — the caller
	// supplies the band so this package stays plan-free.
	BlastHigh bool
}

// Factor records one contributing rule and its point value. The entries
// are plain data so they serialise into the plan artifact and audit
// output unchanged.
type Factor struct {
	// Rule names the factor class ("irreversible_steps",
	// "destructive_actions", "blast_radius", "rollback_coverage",
	// "batch_fanout").
	Rule string `json:"rule"`

	// Points is the contribution of this rule to the total score.
	Points int `json:"points"`

	// Detail explains the trigger count (e.g. "3 irreversible steps" or
	// "23 affected targets (medium band)"). Human-readable, not parsed.
	Detail string `json:"detail"`
}

// Assessment is the complete scoring result for one plan.
type Assessment struct {
	// Score is the aggregate 0-100 danger score (higher = riskier).
	Score int `json:"score"`

	// Factors lists every rule that contributed points, ordered by rule
	// name for stable serialisation. Rules that contributed zero are
	// omitted.
	Factors []Factor `json:"factors,omitempty"`

	// ApprovalFloor is the minimum approval tier this plan must clear:
	// derived from the score bands AND from the hard red lines (any
	// irreversible step ⇒ at least high). The approval routing takes
	// max(workflow declaration, ApprovalFloor) — the floor can raise
	// but never lower the tier.
	ApprovalFloor string `json:"approval_floor"`

	// IrreversibleSteps is the count of irreversible steps. Carried
	// separately because it is the R4 hard signal, not just a score
	// input.
	IrreversibleSteps int `json:"irreversible_steps"`
}

// Score bands for the floor derivation (before red-line overrides):
// 0-39 standard, 40-69 high, 70+ emergency.
const (
	bandHighMax     = 39
	bandEmergencyAt = 70
)

// Assessor scores plans. Stateless; safe for concurrent use.
type Assessor struct{}

// NewAssessor returns a ready Assessor.
func NewAssessor() *Assessor { return &Assessor{} }

// Assess scores the input and derives the approval floor. An empty
// input yields a zero assessment with a standard floor (nothing to
// execute ⇒ nothing to gate).
func (a *Assessor) Assess(in Input) Assessment {
	var factors []Factor
	score := 0

	// Factor 1: irreversible steps (R4 hard signal).
	irreversible := 0
	for _, s := range in.Steps {
		if s.Irreversible {
			irreversible++
		}
	}
	if irreversible > 0 {
		pts := min2(irreversible*pointsIrreversibleStep, maxIrreversiblePoints)
		score += pts
		factors = append(factors, Factor{
			Rule:   "irreversible_steps",
			Points: pts,
			Detail: fmt.Sprintf("%d irreversible step(s) stamped by the plan generator", irreversible),
		})
	}

	// Factor 2: destructive action lexicon.
	destructive := 0
	for _, s := range in.Steps {
		if isDestructiveAction(s.Action) {
			destructive++
		}
	}
	if destructive > 0 {
		pts := min2(destructive*pointsDestructiveAction, maxDestructivePoints)
		score += pts
		factors = append(factors, Factor{
			Rule:   "destructive_actions",
			Points: pts,
			Detail: fmt.Sprintf("%d step(s) with destructive action names (remove/delete/drop/...)", destructive),
		})
	}

	// Factor 3: blast radius bands.
	total := in.DirectTargets + in.IndirectTargets
	var blastBand string
	switch {
	case total > 50:
		blastBand = "high"
	case total > 9:
		blastBand = "medium"
	default:
		blastBand = "low"
	}
	if blastBand == "medium" || blastBand == "high" {
		bands := 1
		if blastBand == "high" {
			bands = 2
		}
		pts := bands * pointsPerBlastBand
		score += pts
		factors = append(factors, Factor{
			Rule:   "blast_radius",
			Points: pts,
			Detail: fmt.Sprintf("%d affected target(s) (%s band)", total, blastBand),
		})
	}

	// Factor 4: rollback coverage — steps with no rollback at all.
	noRollback := 0
	for _, s := range in.Steps {
		if !s.HasRollback {
			noRollback++
		}
	}
	if noRollback > 0 {
		pts := min2(noRollback*pointsNoRollbackStep, maxNoRollbackPoints)
		score += pts
		factors = append(factors, Factor{
			Rule:   "rollback_coverage",
			Points: pts,
			Detail: fmt.Sprintf("%d step(s) declare no rollback", noRollback),
		})
	}

	// Factor 5: batch fan-out.
	if n := in.BatchCount; n > 1 {
		pts := min2((n-1)*pointsPerBatch, maxBatchPoints)
		score += pts
		factors = append(factors, Factor{
			Rule:   "batch_fanout",
			Points: pts,
			Detail: fmt.Sprintf("%d sequential batches", n),
		})
	}

	// Stable ordering for deterministic serialisation.
	sort.Slice(factors, func(i, j int) bool { return factors[i].Rule < factors[j].Rule })

	// Clamp to 0-100 (the caps make overflow unlikely; the clamp is a
	// boundary guarantee, not a tuned value).
	if score > 100 {
		score = 100
	}

	// Floor derivation: bands first...
	floor := LevelStandard
	switch {
	case score >= bandEmergencyAt:
		floor = LevelEmergency
	case score > bandHighMax:
		floor = LevelHigh
	}

	// ...then red-line overrides. R4: any irreversible step demands at
	// least high review, regardless of what the band maths says. The
	// emergency floor additionally requires the conjunction of an
	// irreversible step AND a high-band blast radius (a single
	// irreversible action on one host is exactly the "high" case the
	// spec describes).
	if irreversible > 0 {
		if floor == LevelStandard {
			floor = LevelHigh
		}
		if in.BlastHigh {
			floor = LevelEmergency
		}
	}

	return Assessment{
		Score:             score,
		Factors:           factors,
		ApprovalFloor:     floor,
		IrreversibleSteps: irreversible,
	}
}

// destructiveLexicon matches action names that destroy state. Matched
// case-insensitively against the action segment after the module
// prefix (e.g. "drop" in "mysql.drop_table" — though that action does
// not exist, the lexicon is name-based defence in depth and never the
// only guard).
var destructiveLexicon = []string{
	"remove", "delete", "drop", "truncate", "destroy", "purge", "wipe",
}

func isDestructiveAction(action string) bool {
	if action == "" {
		return false
	}
	seg := strings.ToLower(action)
	// Strip a module prefix if present ("pkg.remove" -> "remove").
	if idx := strings.IndexByte(seg, '.'); idx >= 0 {
		seg = seg[idx+1:]
	}
	for _, w := range destructiveLexicon {
		if strings.Contains(seg, w) {
			return true
		}
	}
	return false
}

// min2 returns the smaller of a, b.
func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MaxLevel returns the higher of two approval levels under the tier
// ordering standard < high < emergency. Unknown levels sort below
// standard (the caller's validated inputs make this a defensive path
// only). This is the routing primitive: tier = MaxLevel(declared, floor).
func MaxLevel(a, b string) string {
	order := map[string]int{LevelStandard: 0, LevelHigh: 1, LevelEmergency: 2}
	// Unknown strings (e.g. a legacy "normal" priority) treat as standard.
	if _, ok := order[a]; !ok {
		a = LevelStandard
	}
	if _, ok := order[b]; !ok {
		b = LevelStandard
	}
	if order[a] >= order[b] {
		return a
	}
	return b
}
