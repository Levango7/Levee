// Package plan computes the blast radius of an executable Plan and
// provides a stable content-addressed hash for plan locking.
//
// The ImpactAnalyzer walks a Plan and produces an ImpactReport that
// distinguishes directly changed targets from indirectly affected ones
// and classifies the overall risk tier (low / medium / high) from the
// total affected count. The report feeds the plan hash (hash.go) and
// the apply-phase risk gate.
package plan

import "sort"

// ImpactReport describes the blast radius of a Plan: which targets are
// directly changed, which are indirectly affected, the total affected
// count and a coarse risk level derived from the total.
type ImpactReport struct {
	// DirectTargets is the deduplicated, sorted list of targets that the
	// plan changes directly (every target across all batches).
	DirectTargets []string

	// IndirectTargets is the deduplicated, sorted list of targets that
	// are indirectly affected by the change but not directly changed.
	// Indirect targets are declared explicitly via the
	// "indirect_targets" field in a step's Args (a []string, []any of
	// strings, or a single string). Targets already in DirectTargets
	// are excluded.
	IndirectTargets []string

	// TotalAffected is the total number of distinct targets affected,
	// directly or indirectly: len(DirectTargets) + len(IndirectTargets).
	TotalAffected int

	// RiskLevel is a coarse risk tier derived from TotalAffected:
	// "low" (< 10), "medium" (10-50 inclusive), "high" (> 50).
	RiskLevel string
}

// Risk level constants. Kept as strings to match the spec wording and
// to make the report directly JSON-serializable without a custom
// marshaller.
const (
	RiskLevelLow    = "low"
	RiskLevelMedium = "medium"
	RiskLevelHigh   = "high"
)

// ImpactAnalyzer computes the blast radius of a Plan. The zero value is
// not ready — use NewImpactAnalyzer. An ImpactAnalyzer is stateless and
// safe for concurrent use.
type ImpactAnalyzer struct{}

// NewImpactAnalyzer returns a ready-to-use ImpactAnalyzer.
func NewImpactAnalyzer() *ImpactAnalyzer {
	return &ImpactAnalyzer{}
}

// Analyze computes the ImpactReport for the given Plan. Direct targets
// are every host appearing in any batch (deduplicated). Indirect
// targets are hosts declared via the "indirect_targets" field in any
// step's Args that are not already direct targets. Both lists are
// returned sorted. TotalAffected is the sum of distinct direct and
// indirect counts. RiskLevel is derived from TotalAffected per the
// thresholds < 10 = low, 10-50 = medium, > 50 = high.
//
// Returns nil when plan is nil.
func (a *ImpactAnalyzer) Analyze(plan *Plan) *ImpactReport {
	if plan == nil {
		return nil
	}

	// Collect direct targets: every host in every batch, deduplicated.
	directSet := make(map[string]struct{})
	for _, b := range plan.Batches {
		for _, t := range b.Targets {
			directSet[t] = struct{}{}
		}
	}

	// Collect indirect targets declared in step Args, excluding any
	// target that is already a direct target.
	indirectSet := make(map[string]struct{})
	for _, b := range plan.Batches {
		for _, s := range b.Steps {
			for _, t := range extractIndirectTargets(s.Args) {
				if _, isDirect := directSet[t]; !isDirect {
					indirectSet[t] = struct{}{}
				}
			}
		}
	}

	directList := sortedKeys(directSet)
	indirectList := sortedKeys(indirectSet)
	total := len(directSet) + len(indirectSet)

	return &ImpactReport{
		DirectTargets:   directList,
		IndirectTargets: indirectList,
		TotalAffected:   total,
		RiskLevel:       riskLevel(total),
	}
}

// extractIndirectTargets reads the "indirect_targets" field from a
// step's Args. The field may be a []string, a []any whose elements are
// strings, or a single string. Any other type yields no targets. nil
// args yield no targets. Non-string elements in []any are skipped.
func extractIndirectTargets(args map[string]any) []string {
	if args == nil {
		return nil
	}
	v, ok := args["indirect_targets"]
	if !ok {
		return nil
	}
	switch val := v.(type) {
	case []string:
		return val
	case []any:
		out := make([]string, 0, len(val))
		for _, e := range val {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{val}
	default:
		return nil
	}
}

// sortedKeys returns the keys of m as a sorted slice. Returns a non-nil
// empty slice when m is empty so that JSON marshalling produces "[]"
// rather than "null".
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// riskLevel maps a total affected count to a risk tier:
// < 10 → "low", 10-50 → "medium", > 50 → "high".
func riskLevel(total int) string {
	switch {
	case total < 10:
		return RiskLevelLow
	case total <= 50:
		return RiskLevelMedium
	default:
		return RiskLevelHigh
	}
}
