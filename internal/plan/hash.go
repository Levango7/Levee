// Package plan provides a stable content-addressed hash for plan locking.
//
// ComputeHash canonicalizes a Plan into an order-stable JSON form and
// returns its SHA-256 digest. The canonical form includes the workflow
// name, the sorted target set, the batch structure (indices, sorted
// per-batch targets, steps with sorted args), and the impact report
// (direct + indirect targets). The hash is hex-encoded.
//
// Canonicalization guarantees that target order within a batch does not
// affect the hash (sets are sorted), while batch order and step order
// are preserved (they carry execution semantics). Map keys (step args)
// are serialised in lexicographic order by encoding/json.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// ComputeHash returns the SHA-256 hash of the canonical representation
// of the plan. The canonical form includes:
//   - workflow_name
//   - the sorted, deduplicated target set across all batches
//   - the batch list (index, sorted per-batch targets, steps, max
//     concurrency) — batch order is preserved
//   - the step list per batch (name, module, action, args) — step order
//     is preserved; args map keys are sorted by encoding/json
//   - the impact report (sorted direct + indirect targets)
//
// The hash is lowercase hex-encoded (64 chars). Returns the empty
// string when plan is nil or canonical serialization fails.
func ComputeHash(plan *Plan) string {
	if plan == nil {
		return ""
	}
	data, err := json.Marshal(buildCanonical(plan))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifyHash reports whether the computed hash of the plan equals the
// expected hash. Returns false when plan is nil, when the expected
// string is empty, or when the hashes differ. The comparison is a plain
// string equality (both sides are hex-encoded digests of equal length,
// so timing attack is not a concern for plan locking; use
// crypto/subtle.ConstantTimeCompare if constant-time is required).
func VerifyHash(plan *Plan, expected string) bool {
	if plan == nil || expected == "" {
		return false
	}
	return ComputeHash(plan) == expected
}

// canonicalPlan is the order-stable representation of a Plan used for
// hashing. All target slices are sorted; all maps rely on
// encoding/json's lexicographic key ordering for stability. Batch and
// step slices preserve their order (execution semantics).
type canonicalPlan struct {
	WorkflowName string           `json:"workflow_name"`
	Targets      []string         `json:"targets"`
	Batches      []canonicalBatch `json:"batches"`
	Impact       canonicalImpact  `json:"impact"`
}

// canonicalBatch is the order-stable representation of a Batch. Targets
// are sorted (in-batch execution is concurrent, so order is not
// semantic). Steps preserve order (step sequence is semantic).
type canonicalBatch struct {
	Index          int             `json:"index"`
	Targets        []string        `json:"targets"`
	Steps          []canonicalStep `json:"steps"`
	MaxConcurrency int             `json:"max_concurrency"`
}

// canonicalStep is the order-stable representation of a PlanStep. Args
// is a map[string]any whose top-level keys are sorted by encoding/json;
// nested maps are recursively canonicalized so their keys are sorted
// too. Rollback / Approval / Gate are not included in the hash because
// they are runtime directives, not plan identity — two plans that differ
// only in approval routing are considered the same change.
type canonicalStep struct {
	Name   string         `json:"name"`
	Module string         `json:"module"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// canonicalImpact is the order-stable representation of the impact
// report. Both target lists are already sorted by ImpactAnalyzer.
type canonicalImpact struct {
	DirectTargets   []string `json:"direct_targets"`
	IndirectTargets []string `json:"indirect_targets"`
}

// buildCanonical constructs the canonical representation of the plan.
// The global target set is deduplicated and sorted; per-batch targets
// are sorted (preserving set membership); steps preserve order; args
// are recursively canonicalized so nested map keys are stable.
func buildCanonical(plan *Plan) canonicalPlan {
	// Global target set across all batches (deduplicated, sorted).
	targetSet := make(map[string]struct{})
	for _, b := range plan.Batches {
		for _, t := range b.Targets {
			targetSet[t] = struct{}{}
		}
	}

	// Canonical batches preserve order.
	batches := make([]canonicalBatch, len(plan.Batches))
	for i, b := range plan.Batches {
		batches[i] = canonicalBatch{
			Index:          b.Index,
			Targets:        sortedCopy(b.Targets),
			Steps:          canonicalSteps(b.Steps),
			MaxConcurrency: b.MaxConcurrency,
		}
	}

	// Impact component from the analyzer (already sorted).
	var impact canonicalImpact
	if r := NewImpactAnalyzer().Analyze(plan); r != nil {
		impact = canonicalImpact{
			DirectTargets:   r.DirectTargets,
			IndirectTargets: r.IndirectTargets,
		}
	}

	return canonicalPlan{
		WorkflowName: plan.WorkflowName,
		Targets:      sortedKeys(targetSet),
		Batches:      batches,
		Impact:       impact,
	}
}

// canonicalSteps converts plan steps to canonical steps, preserving
// step order. Args are deep-canonicalized via canonicalArgs so nested
// map keys are stabilized.
func canonicalSteps(steps []PlanStep) []canonicalStep {
	out := make([]canonicalStep, len(steps))
	for i, s := range steps {
		out[i] = canonicalStep{
			Name:   s.Name,
			Module: s.Module,
			Action: s.Action,
			Args:   canonicalArgs(s.Args),
		}
	}
	return out
}

// canonicalArgs returns a deep-canonicalized copy of args: nested
// map[string]any values are rebuilt with canonicalized values so that
// encoding/json emits their keys in sorted order, and []any slices are
// rebuilt with canonicalized elements. Scalar values are passed through
// unchanged. nil args return nil (so the field marshals to "null",
// distinguishing an absent args map from an empty one).
func canonicalArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = canonicalValue(v)
	}
	return out
}

// canonicalValue recursively canonicalizes a value: maps are rebuilt as
// map[string]any with canonicalized values, []any slices are rebuilt
// with canonicalized elements, everything else is passed through. This
// ensures encoding/json produces stable output for nested structures.
func canonicalValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return canonicalArgs(val)
	case []any:
		out := make([]any, len(val))
		for i, e := range val {
			out[i] = canonicalValue(e)
		}
		return out
	default:
		return v
	}
}

// sortedCopy returns a sorted copy of the given slice. The input is
// left unchanged. Returns a non-nil empty slice when input is empty so
// that JSON marshalling produces "[]" rather than "null".
func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
