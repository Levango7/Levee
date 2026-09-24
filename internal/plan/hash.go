// Package plan provides versioned, content-addressed hashes for plan
// locking. ComputeHash emits the current v2 digest; VerifyHash accepts both
// v2 governance-aware hashes and legacy 64-hex v1 hashes so persisted plans
// remain executable after upgrade. Re-planning is required to migrate a v1
// artifact to v2 governance coverage.
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/nexus/levee/internal/dsl"
)

const (
	// HashVersionV2 is the current plan hash version. V2 includes the
	// approval, rollback, verification-gate and risk semantics that V1 left
	// outside the canonical form.
	HashVersionV2 = "v2"
	// HashVersionV1 is the legacy execution-only hash format.
	HashVersionV1 = "v1"
	hashPrefixV2  = HashVersionV2 + ":"
	legacyHashLen = 64
)

// ComputeHash returns the current versioned SHA-256 plan hash (v2:<hex>).
// Returns the empty string when plan is nil or canonical serialization fails.
func ComputeHash(plan *Plan) string {
	if plan == nil {
		return ""
	}
	return hashPrefixV2 + hashCanonical(buildCanonicalV2(plan))
}

// ComputeHashV1 returns the legacy 64-hex execution-only hash. It exists for
// upgrade verification and regression fixtures; new code should use
// ComputeHash / VerifyHash.
func ComputeHashV1(plan *Plan) string {
	if plan == nil {
		return ""
	}
	return hashCanonical(buildCanonicalV1(plan))
}

func hashCanonical(canonical any) string {
	data, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifyHash reports whether the plan matches the version encoded by
// expected. V2 hashes cover governance fields; bare 64-hex expected values
// are verified with the legacy V1 algorithm. Unknown/malformed versions fail.
func VerifyHash(plan *Plan, expected string) bool {
	if plan == nil || expected == "" {
		return false
	}
	switch {
	case strings.HasPrefix(expected, hashPrefixV2):
		return ComputeHash(plan) == expected
	case len(expected) == legacyHashLen && isLowerHex(expected):
		return ComputeHashV1(plan) == expected
	default:
		return false
	}
}

func isLowerHex(value string) bool {
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// canonicalPlan is the legacy V1 execution-only representation. V2 embeds
// this unchanged and adds governance fields; the V1 builder remains
// byte-for-byte compatible with already-persisted 64-hex hashes.
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

// canonicalStep is the V1 execution step. Governance directives are added
// by canonicalStepV2 below.
type canonicalStep struct {
	Name   string         `json:"name"`
	Module string         `json:"module"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

// canonicalImpact is the order-stable representation of the impact report.
// Both target lists are already sorted by ImpactAnalyzer.
type canonicalImpact struct {
	DirectTargets   []string `json:"direct_targets"`
	IndirectTargets []string `json:"indirect_targets"`
}

// canonicalPlanV2 extends the execution identity with every governance
// directive that is persisted with a plan and can alter approval,
// compensation or verification behaviour.
type canonicalPlanV2 struct {
	Version       string                `json:"hash_version"`
	WorkflowName  string                `json:"workflow_name"`
	Targets       []string              `json:"targets"`
	Batches       []canonicalBatchV2    `json:"batches"`
	Impact        canonicalImpact       `json:"impact"`
	RiskScore     int                   `json:"risk_score"`
	RiskFactors   []canonicalRiskFactor `json:"risk_factors"`
	ApprovalFloor string                `json:"approval_floor"`
	Approval      *canonicalApproval    `json:"approval,omitempty"`
	Rollback      *canonicalRollback    `json:"rollback,omitempty"`
	Gate          *canonicalGate        `json:"gate,omitempty"`
}

type canonicalBatchV2 struct {
	Index          int               `json:"index"`
	Targets        []string          `json:"targets"`
	Steps          []canonicalStepV2 `json:"steps"`
	MaxConcurrency int               `json:"max_concurrency"`
	Gate           *canonicalGate    `json:"gate,omitempty"`
}

type canonicalRiskFactor struct {
	Rule   string `json:"rule"`
	Points int    `json:"points"`
	Detail string `json:"detail"`
}

type canonicalStepV2 struct {
	Execution          canonicalStep      `json:"execution"`
	Rollback           *canonicalRollback `json:"rollback,omitempty"`
	Approval           *canonicalApproval `json:"approval,omitempty"`
	Gate               *canonicalGate     `json:"gate,omitempty"`
	Irreversible       bool               `json:"irreversible"`
	IrreversibleReason string             `json:"irreversible_reason,omitempty"`
}

type canonicalRollback struct {
	Strategy      string                  `json:"strategy"`
	OnFailure     string                  `json:"on_failure"`
	VerifyAfter   bool                    `json:"verify_after"`
	SnapshotPaths []string                `json:"snapshot_paths"`
	Steps         []canonicalRollbackStep `json:"steps"`
}

type canonicalRollbackStep struct {
	Name   string         `json:"name"`
	Module string         `json:"module"`
	Action string         `json:"action"`
	Args   map[string]any `json:"args"`
}

type canonicalApproval struct {
	Level            string   `json:"level"`
	Approvers        []string `json:"approvers"`
	Timeout          string   `json:"timeout"`
	MinApprovers     int      `json:"min_approvers"`
	ExcludeInitiator bool     `json:"exclude_initiator"`
}

type canonicalGate struct {
	Pre   []canonicalGateCheck `json:"pre"`
	Post  []canonicalGateCheck `json:"post"`
	Batch []canonicalGateCheck `json:"batch"`
}

type canonicalGateCheck struct {
	Type         string         `json:"type"`
	Command      string         `json:"command"`
	ExpectExit   int            `json:"expect_exit"`
	ExpectStdout string         `json:"expect_stdout"`
	Source       string         `json:"source"`
	Timeout      string         `json:"timeout"`
	Params       map[string]any `json:"params"`
}

// buildCanonicalV1 constructs the legacy execution-only representation.
// Do not reorder fields or change its JSON tags: persisted V1 hashes depend
// on the exact bytes produced here.
func buildCanonical(plan *Plan) canonicalPlan {
	return buildCanonicalV1(plan)
}

func buildCanonicalV1(plan *Plan) canonicalPlan {
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

// canonicalSteps converts plan steps to V1 execution steps, preserving
// step order and recursively stabilising map keys.
func canonicalSteps(steps []PlanStep) []canonicalStep {
	return canonicalStepsV1(steps)
}

func canonicalStepsV1(steps []PlanStep) []canonicalStep {
	out := make([]canonicalStep, len(steps))
	for i, step := range steps {
		out[i] = canonicalStep{
			Name: step.Name, Module: step.Module, Action: step.Action, Args: canonicalArgs(step.Args),
		}
	}
	return out
}

func buildCanonicalV2(plan *Plan) canonicalPlanV2 {
	v1 := buildCanonicalV1(plan)
	batches := make([]canonicalBatchV2, len(plan.Batches))
	for i, batch := range plan.Batches {
		batches[i] = canonicalBatchV2{
			Index: batch.Index, Targets: sortedCopy(batch.Targets),
			Steps: canonicalStepsV2(batch.Steps), MaxConcurrency: batch.MaxConcurrency,
			Gate: canonicalGateV2(batch.Gate),
		}
	}
	return canonicalPlanV2{
		Version: HashVersionV2, WorkflowName: v1.WorkflowName, Targets: v1.Targets,
		Batches: batches, Impact: v1.Impact, RiskScore: plan.RiskScore,
		RiskFactors: canonicalRiskFactors(plan.RiskFactors), ApprovalFloor: plan.ApprovalFloor,
		Approval: canonicalApprovalV2(plan.Approval), Rollback: canonicalRollbackV2(plan.Rollback),
		Gate: canonicalGateV2(plan.Gate),
	}
}

func canonicalRiskFactors(in []RiskFactor) []canonicalRiskFactor {
	out := make([]canonicalRiskFactor, len(in))
	for i, factor := range in {
		out[i] = canonicalRiskFactor(factor)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		if out[i].Points != out[j].Points {
			return out[i].Points < out[j].Points
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func canonicalStepsV2(steps []PlanStep) []canonicalStepV2 {
	out := make([]canonicalStepV2, len(steps))
	for i, step := range steps {
		out[i] = canonicalStepV2{
			Execution: canonicalStepsV1([]PlanStep{step})[0],
			Rollback:  canonicalRollbackV2(step.Rollback), Approval: canonicalApprovalV2(step.Approval),
			Gate: canonicalGateV2(step.Gate), Irreversible: step.Irreversible,
			IrreversibleReason: step.IrreversibleReason,
		}
	}
	return out
}

func canonicalRollbackV2(spec *dsl.RollbackSpec) *canonicalRollback {
	if spec == nil {
		return nil
	}
	steps := make([]canonicalRollbackStep, len(spec.Steps))
	for i, step := range spec.Steps {
		steps[i] = canonicalRollbackStep{
			Name: step.Name, Module: step.Module, Action: step.Action, Args: canonicalArgs(step.Args),
		}
	}
	return &canonicalRollback{
		Strategy: spec.Strategy, OnFailure: spec.OnFailure, VerifyAfter: spec.VerifyAfter,
		SnapshotPaths: sortedCopy(spec.SnapshotPaths), Steps: steps,
	}
}

func canonicalApprovalV2(spec *dsl.ApprovalSpec) *canonicalApproval {
	if spec == nil {
		return nil
	}
	return &canonicalApproval{
		Level: spec.Level, Approvers: sortedCopy(spec.Approvers), Timeout: spec.Timeout,
		MinApprovers: spec.MinApprovers, ExcludeInitiator: spec.ExcludeInitiator,
	}
}

func canonicalGateV2(spec *dsl.GateSpec) *canonicalGate {
	if spec == nil {
		return nil
	}
	return &canonicalGate{
		Pre: canonicalGateChecks(spec.Pre), Post: canonicalGateChecks(spec.Post), Batch: canonicalGateChecks(spec.Batch),
	}
}

func canonicalGateChecks(in []dsl.GateCheck) []canonicalGateCheck {
	out := make([]canonicalGateCheck, len(in))
	for i, check := range in {
		out[i] = canonicalGateCheck{
			Type: check.Type, Command: check.Command, ExpectExit: check.ExpectExit,
			ExpectStdout: check.ExpectStdout, Source: check.Source, Timeout: check.Timeout,
			Params: canonicalArgs(check.Params),
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
