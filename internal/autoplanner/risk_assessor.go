// Package autoplanner converts a recommend.Recommendation into an executable
// LEVEELang workflow. It bridges the AI recommendation engine (Phase B) and
// the plan execution pipeline (Phase A) by:
//
//   - Assessing the risk of a recommendation and mapping it to an approval
//     tier (standard / high / emergency).
//   - Parsing the recommendation's LEVEELang YAML draft into structured
//     steps.
//   - Dividing the steps into batches based on the target topology.
//   - Estimating the total execution time.
//   - Emitting a formal Workflow value ready for the apply phase.
//
// The AutoPlanner is stateless and safe for concurrent use. It never panics;
// errors are propagated through error returns.
package autoplanner

// risk_assessor.go implements the RiskAssessor that maps a
// recommend.RiskLevel to an approval tier and decides whether a
// recommendation may be auto-executed. The assessor delegates the tier
// selection to approval.LevelManager so that the approval policy stays
// single-sourced.
//
// The mapping is:
//
//	RiskLow      -> LevelStandard,  CanAutoExecute = true
//	RiskMedium   -> LevelHigh,     CanAutoExecute = false
//	RiskHigh     -> LevelHigh,     CanAutoExecute = false
//	RiskCritical -> LevelEmergency, CanAutoExecute = false
//
// The RiskAssessor is stateless and safe for concurrent use. It never
// panics; a nil recommendation yields a conservative default assessment.

import (
	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/recommend"
)

// --- Assessment --------------------------------------------------------------

// Assessment is the output of RiskAssessor.Assess. It captures the original
// risk level, the mapped approval tier, the confidence in the recommendation,
// whether the workflow may be executed without explicit human approval, and
// a list of human-readable reasons explaining the decision.
type Assessment struct {
	// RiskLevel is the original recommend.RiskLevel of the recommendation.
	RiskLevel recommend.RiskLevel

	// ApprovalLevel is the mapped approval tier: approval.LevelStandard,
	// approval.LevelHigh or approval.LevelEmergency.
	ApprovalLevel string

	// Confidence is the recommendation's confidence score in [0, 1].
	Confidence float64

	// CanAutoExecute reports whether the workflow may be applied without
	// explicit human approval. Only low-risk recommendations may be
	// auto-executed.
	CanAutoExecute bool

	// Reasons is a list of human-readable explanations for the assessment,
	// suitable for display in the audit log.
	Reasons []string
}

// --- RiskAssessor ------------------------------------------------------------

// RiskAssessor maps a recommend.RiskLevel to an approval tier and decides
// whether a recommendation may be auto-executed. It delegates the tier
// selection to approval.LevelManager so that the approval policy stays
// single-sourced.
//
// RiskAssessor is stateless and safe for concurrent use.
type RiskAssessor struct {
	levelMgr *approval.LevelManager
}

// NewRiskAssessor returns a RiskAssessor wired to a default LevelManager.
func NewRiskAssessor() *RiskAssessor {
	return &RiskAssessor{levelMgr: approval.NewLevelManager()}
}

// Assess evaluates the risk of rec and returns an Assessment describing the
// approval tier and auto-execute eligibility. The confidence and risk level
// are copied from rec; the reasons explain the mapping.
//
// A nil recommendation yields a conservative assessment: standard tier with
// auto-execute disabled, so that a nil pointer can never trigger an
// unattended change.
func (r *RiskAssessor) Assess(rec *recommend.Recommendation) Assessment {
	if rec == nil {
		return Assessment{
			RiskLevel:      recommend.RiskLow,
			ApprovalLevel:  approval.LevelStandard,
			CanAutoExecute: false,
			Reasons:        []string{"nil recommendation: defaulting to standard tier with auto-execute disabled"},
		}
	}

	// Build an approval.Step whose Irreversible / Emergency flags encode
	// the risk level. LevelManager.DetermineLevelName maps the flags to
	// the three tiers: emergency > irreversible > standard.
	step := approval.Step{
		Module: "autoplanner",
		Action: "fix",
	}

	var reasons []string
	var canAuto bool

	switch rec.RiskLevel {
	case recommend.RiskLow:
		// Reversible: standard tier, auto-execute allowed.
		canAuto = true
		reasons = append(reasons, "risk=low: reversible change, standard approval tier, auto-execute enabled")
	case recommend.RiskMedium:
		step.Irreversible = true
		reasons = append(reasons, "risk=medium: may have side effects, high approval tier, auto-execute disabled")
	case recommend.RiskHigh:
		step.Irreversible = true
		reasons = append(reasons, "risk=high: affects availability, high approval tier, auto-execute disabled")
	case recommend.RiskCritical:
		step.Emergency = true
		reasons = append(reasons, "risk=critical: destructive change, emergency approval tier, auto-execute disabled")
	default:
		// Unknown risk level: treat as high (conservative) so the
		// operator must explicitly approve.
		step.Irreversible = true
		reasons = append(reasons, "risk="+string(rec.RiskLevel)+": unknown risk level, defaulting to high approval tier")
	}

	level := r.levelMgr.DetermineLevelName(step)
	reasons = append(reasons, "approval level resolved: "+level)

	return Assessment{
		RiskLevel:      rec.RiskLevel,
		ApprovalLevel:  level,
		Confidence:     rec.Confidence,
		CanAutoExecute: canAuto,
		Reasons:        reasons,
	}
}