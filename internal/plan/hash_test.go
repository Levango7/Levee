package plan

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

// hashPlan builds a plan with one batch, the given targets and steps,
// then returns its hash. Convenience helper for hash tests.
func hashPlan(name string, targets []string, steps ...PlanStep) string {
	return ComputeHash(buildPlan(name, targets, steps...))
}

func assertV2Hash(t *testing.T, hash string) {
	t.Helper()
	require.True(t, strings.HasPrefix(hash, hashPrefixV2), "hash must carry v2 prefix: %q", hash)
	digest := strings.TrimPrefix(hash, hashPrefixV2)
	require.Len(t, digest, 64, "sha256 hex length")
	assert.True(t, isLowerHex(digest), "hash digest must be lowercase hex")
}

// TestComputeHashSamePlan verifies that computing the hash of the same
// plan twice yields the same value (determinism).
func TestComputeHashSamePlan(t *testing.T) {
	plan := buildPlan("wf-stable", []string{"host-a", "host-b"},
		PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade",
			Args: map[string]any{"name": "kernel"}},
	)

	h1 := ComputeHash(plan)
	h2 := ComputeHash(plan)
	require.NotEmpty(t, h1)
	assert.Equal(t, h1, h2, "same plan must yield same hash")
}

// TestComputeHashFormat verifies the versioned v2 SHA-256 hash format.
func TestComputeHashFormat(t *testing.T) {
	plan := buildPlan("wf-fmt", []string{"host-a"})
	assertV2Hash(t, ComputeHash(plan))
}

// TestComputeHashDifferentWorkflowName verifies that two plans differing
// only in workflow name produce different hashes.
func TestComputeHashDifferentWorkflowName(t *testing.T) {
	targets := []string{"host-a", "host-b"}
	h1 := hashPlan("wf-one", targets)
	h2 := hashPlan("wf-two", targets)
	assert.NotEqual(t, h1, h2, "different workflow names must hash differently")
}

// TestComputeHashDifferentTargets verifies that two plans differing only
// in target set produce different hashes.
func TestComputeHashDifferentTargets(t *testing.T) {
	h1 := hashPlan("wf", []string{"host-a", "host-b"})
	h2 := hashPlan("wf", []string{"host-a", "host-c"})
	assert.NotEqual(t, h1, h2, "different target sets must hash differently")
}

// TestComputeHashDifferentArgs verifies that two plans differing only in
// step args produce different hashes (parameter change affects hash).
func TestComputeHashDifferentArgs(t *testing.T) {
	step1 := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"version": "1.0"},
	}
	step2 := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"version": "2.0"},
	}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.NotEqual(t, h1, h2, "different arg values must hash differently")
}

// TestComputeHashArgsKeyOrderStable verifies that the order of keys in a
// step's Args map does not affect the hash (encoding/json sorts map
// keys).
func TestComputeHashArgsKeyOrderStable(t *testing.T) {
	step1 := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"a": "1", "b": "2", "c": "3"},
	}
	step2 := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"c": "3", "a": "1", "b": "2"},
	}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.Equal(t, h1, h2, "arg key order must not affect hash")
}

// TestComputeHashNestedArgsKeyOrderStable verifies that nested map keys
// in args are also order-stable.
func TestComputeHashNestedArgsKeyOrderStable(t *testing.T) {
	step1 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade",
		Args: map[string]any{
			"config": map[string]any{"z": "1", "a": "2"},
		},
	}
	step2 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade",
		Args: map[string]any{
			"config": map[string]any{"a": "2", "z": "1"},
		},
	}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.Equal(t, h1, h2, "nested arg key order must not affect hash")
}

// TestComputeHashTargetOrderStable verifies that the order of targets
// within a batch does not affect the hash (targets are treated as a
// set, sorted in the canonical form).
func TestComputeHashTargetOrderStable(t *testing.T) {
	plan1 := buildPlan("wf", []string{"host-a", "host-b", "host-c"})
	plan2 := buildPlan("wf", []string{"host-c", "host-a", "host-b"})
	plan3 := buildPlan("wf", []string{"host-b", "host-c", "host-a"})

	h1 := ComputeHash(plan1)
	h2 := ComputeHash(plan2)
	h3 := ComputeHash(plan3)
	assert.Equal(t, h1, h2, "target order in batch must not affect hash")
	assert.Equal(t, h1, h3, "target order in batch must not affect hash")
}

// TestComputeHashTargetOrderAcrossBatchesStable verifies that target
// order across batches (when the partition is the same) is captured by
// the sorted global target set, but the per-batch partition is still
// part of the hash. Two plans with the same target set but different
// batch partitions must hash differently.
func TestComputeHashBatchPartitionMatters(t *testing.T) {
	// Same target set, different batch partitions.
	plan1 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a", "h-b"}},
		{Index: 1, Targets: []string{"h-c", "h-d"}},
	})
	plan2 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a"}},
		{Index: 1, Targets: []string{"h-b", "h-c", "h-d"}},
	})
	h1 := ComputeHash(plan1)
	h2 := ComputeHash(plan2)
	assert.NotEqual(t, h1, h2, "different batch partitions must hash differently")
}

// TestComputeHashBatchOrderMatters verifies that the order of batches
// (which batch runs first) affects the hash, because batch order is
// semantic (canary before rollout).
func TestComputeHashBatchOrderMatters(t *testing.T) {
	plan1 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a"}},
		{Index: 1, Targets: []string{"h-b"}},
	})
	plan2 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-b"}},
		{Index: 1, Targets: []string{"h-a"}},
	})
	h1 := ComputeHash(plan1)
	h2 := ComputeHash(plan2)
	assert.NotEqual(t, h1, h2, "different batch orders must hash differently")
}

// TestComputeHashStepOrderMatters verifies that the order of steps
// within a batch affects the hash, because step order is semantic.
func TestComputeHashStepOrderMatters(t *testing.T) {
	stepA := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade"}
	stepB := PlanStep{Name: "verify", Module: "shell", Action: "exec"}
	plan1 := buildPlan("wf", []string{"host-a"}, stepA, stepB)
	plan2 := buildPlan("wf", []string{"host-a"}, stepB, stepA)
	h1 := ComputeHash(plan1)
	h2 := ComputeHash(plan2)
	assert.NotEqual(t, h1, h2, "different step orders must hash differently")
}

// TestComputeHashDifferentStepName verifies that differing step names
// produce different hashes.
func TestComputeHashDifferentStepName(t *testing.T) {
	step1 := PlanStep{Name: "upgrade", Module: "pkg", Action: "upgrade"}
	step2 := PlanStep{Name: "install", Module: "pkg", Action: "upgrade"}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.NotEqual(t, h1, h2, "different step names must hash differently")
}

// TestComputeHashDifferentModuleAction verifies that differing module /
// action produce different hashes.
func TestComputeHashDifferentModuleAction(t *testing.T) {
	step1 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade"}
	step2 := PlanStep{Name: "s", Module: "shell", Action: "exec"}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.NotEqual(t, h1, h2, "different module/action must hash differently")
}

// TestComputeHashDifferentMaxConcurrency verifies that differing
// MaxConcurrency produces different hashes.
func TestComputeHashDifferentMaxConcurrency(t *testing.T) {
	plan1 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a"}, MaxConcurrency: 1},
	})
	plan2 := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a"}, MaxConcurrency: 10},
	})
	h1 := ComputeHash(plan1)
	h2 := ComputeHash(plan2)
	assert.NotEqual(t, h1, h2, "different max concurrency must hash differently")
}

// TestComputeHashNilArgsVsEmptyArgs verifies that nil args and empty
// args produce different hashes (null vs {} in JSON).
func TestComputeHashNilArgsVsEmptyArgs(t *testing.T) {
	step1 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade", Args: nil}
	step2 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade", Args: map[string]any{}}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.NotEqual(t, h1, h2, "nil args vs empty args must hash differently")
}

// TestComputeHashNilPlan verifies that a nil plan yields an empty hash.
func TestComputeHashNilPlan(t *testing.T) {
	assert.Equal(t, "", ComputeHash(nil))
}

// TestComputeHashEmptyPlan verifies that a plan with no batches and no
// workflow name still produces a stable non-empty hash.
func TestComputeHashEmptyPlan(t *testing.T) {
	plan := &Plan{ID: "p", WorkflowName: "", Batches: nil}
	h := ComputeHash(plan)
	assert.NotEmpty(t, h, "empty plan should still hash")
	assertV2Hash(t, h)
}

// TestComputeHashIndirectTargetsAffectHash verifies that indirect
// targets (declared via step args) are part of the impact component and
// affect the hash.
func TestComputeHashIndirectTargetsAffectHash(t *testing.T) {
	step1 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"indirect_targets": []string{"down-1"}},
	}
	step2 := PlanStep{Name: "s", Module: "pkg", Action: "upgrade",
		Args: map[string]any{"indirect_targets": []string{"down-2"}},
	}
	h1 := hashPlan("wf", []string{"host-a"}, step1)
	h2 := hashPlan("wf", []string{"host-a"}, step2)
	assert.NotEqual(t, h1, h2, "different indirect targets must hash differently")
}

// TestVerifyHashMatch verifies VerifyHash returns true for a matching
// hash.
func TestVerifyHashMatch(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	h := ComputeHash(plan)
	assert.True(t, VerifyHash(plan, h))
}

// TestVerifyHashMismatch verifies VerifyHash returns false for a
// non-matching hash.
func TestVerifyHashMismatch(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	assert.False(t, VerifyHash(plan, "deadbeef"))
}

// TestVerifyHashEmptyExpected verifies VerifyHash returns false when
// the expected hash is empty.
func TestVerifyHashEmptyExpected(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	assert.False(t, VerifyHash(plan, ""))
}

// TestVerifyHashNilPlan verifies VerifyHash returns false when the plan
// is nil.
func TestVerifyHashNilPlan(t *testing.T) {
	assert.False(t, VerifyHash(nil, "somethash"))
}

// TestVerifyHashAfterMutation verifies that mutating a plan after
// computing its hash causes VerifyHash to fail (the hash no longer
// matches the mutated plan).
func TestVerifyHashAfterMutation(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	h := ComputeHash(plan)
	require.True(t, VerifyHash(plan, h))

	// Mutate: add a target.
	plan.Batches[0].Targets = append(plan.Batches[0].Targets, "host-b")
	plan.TotalTargets = 2
	assert.False(t, VerifyHash(plan, h), "mutated plan must not verify against old hash")
}

// TestComputeHashLargePlan verifies hashing works on a larger plan and
// is deterministic.
func TestComputeHashLargePlan(t *testing.T) {
	targets := make([]string, 100)
	for i := range targets {
		targets[i] = fmt.Sprintf("host-%03d", i)
	}
	steps := []PlanStep{
		{Name: "upgrade", Module: "pkg", Action: "upgrade",
			Args: map[string]any{"name": "kernel", "version": "5.15"}},
		{Name: "verify", Module: "shell", Action: "exec"},
	}
	plan := buildPlan("wf-large", targets, steps...)

	h1 := ComputeHash(plan)
	h2 := ComputeHash(plan)
	assert.NotEmpty(t, h1)
	assert.Equal(t, h1, h2)
	assertV2Hash(t, h1)
}

// TestComputeHashMultiBatchDeterministic verifies that a multi-batch
// plan hashes deterministically across repeated calls.
func legacyGoldenPlan() *Plan {
	return &Plan{
		ID: "ignored", WorkflowName: "legacy-wf", TotalTargets: 2,
		CreatedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Batches: []Batch{{
			Index: 0, Targets: []string{"host-b", "host-a"}, MaxConcurrency: 2,
			Steps: []PlanStep{{Name: "exec", Module: "shell", Action: "exec", Args: map[string]any{"z": 2, "a": 1}}},
		}},
	}
}

func TestVerifyHashLegacyV1(t *testing.T) {
	plan := buildPlan("legacy", []string{"host-a"})
	legacy := ComputeHashV1(plan)
	require.Len(t, legacy, 64)
	assert.True(t, VerifyHash(plan, legacy), "persisted v1 plans must remain executable")
}

func TestComputeHashV1GoldenCompatibility(t *testing.T) {
	const golden = "cf0eaad0689171cb20099c38b378c04a20ecbfd5831e769a3a70515012ba0391"
	plan := legacyGoldenPlan()
	assert.Equal(t, golden, ComputeHashV1(plan), "legacy canonical bytes must remain stable")
	assert.True(t, VerifyHash(plan, golden))
}

func TestVerifyHashRejectsUnknownVersion(t *testing.T) {
	plan := buildPlan("unknown", []string{"host-a"})
	assert.False(t, VerifyHash(plan, "v3:"+strings.Repeat("a", 64)))
	assert.False(t, VerifyHash(plan, strings.ToUpper(ComputeHashV1(plan))))
}

func TestComputeHashGovernanceFieldsAffectV2(t *testing.T) {
	base := buildPlan("governance", []string{"host-a"}, PlanStep{
		Name: "restart", Module: "svc", Action: "restart",
		Args:         map[string]any{"name": "nginx"},
		Rollback:     &dsl.RollbackSpec{Strategy: "snapshot", SnapshotPaths: []string{"/etc/b", "/etc/a"}},
		Approval:     &dsl.ApprovalSpec{Level: "high", Approvers: []string{"bob", "alice"}, MinApprovers: 2},
		Gate:         &dsl.GateSpec{Post: []dsl.GateCheck{{Type: "cmd", Command: "true"}}},
		Irreversible: true, IrreversibleReason: "destructive",
	})
	base.RiskScore = 77
	base.Approval = &dsl.ApprovalSpec{Level: "high", Approvers: []string{"charlie"}}
	base.Rollback = &dsl.RollbackSpec{Strategy: "snapshot", SnapshotPaths: []string{"/etc/main.conf"}}
	base.Gate = &dsl.GateSpec{Pre: []dsl.GateCheck{{Type: "cmd", Command: "true"}}}
	base.Batches[0].Gate = &dsl.GateSpec{Batch: []dsl.GateCheck{{Type: "cmd", Command: "true"}}}
	baseline := ComputeHash(base)

	tests := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"risk score", func(p *Plan) { p.RiskScore++ }},
		{"risk factor", func(p *Plan) { p.RiskFactors = []RiskFactor{{Rule: "new", Points: 1}} }},
		{"approval floor", func(p *Plan) { p.ApprovalFloor = "emergency" }},
		{"workflow approval", func(p *Plan) { p.Approval.Level = "emergency" }},
		{"workflow rollback", func(p *Plan) { p.Rollback.Strategy = "undo-action" }},
		{"workflow gate", func(p *Plan) { p.Gate.Pre[0].Command = "false" }},
		{"batch gate", func(p *Plan) { p.Batches[0].Gate.Batch[0].Command = "false" }},
		{"rollback strategy", func(p *Plan) { p.Batches[0].Steps[0].Rollback.Strategy = "undo-action" }},
		{"undo step idempotency", func(p *Plan) {
			p.Batches[0].Steps[0].Rollback.Steps = []dsl.Step{{Name: "undo", Module: "svc", Action: "stop", Idempotent: true}}
		}},
		{"snapshot path", func(p *Plan) { p.Batches[0].Steps[0].Rollback.SnapshotPaths[0] = "/etc/c" }},
		{"approval", func(p *Plan) { p.Batches[0].Steps[0].Approval.MinApprovers = 3 }},
		{"gate", func(p *Plan) { p.Batches[0].Steps[0].Gate.Post[0].Command = "false" }},
		{"irreversible", func(p *Plan) { p.Batches[0].Steps[0].Irreversible = false }},
		{"irreversible reason", func(p *Plan) { p.Batches[0].Steps[0].IrreversibleReason = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(base)
			require.NoError(t, err)
			var clone Plan
			require.NoError(t, json.Unmarshal(raw, &clone))
			tt.mutate(&clone)
			assert.NotEqual(t, baseline, ComputeHash(&clone))
		})
	}
}

func TestComputeHashV2SetOrderingStable(t *testing.T) {
	mk := func(factors []RiskFactor, approvers, paths []string) string {
		p := buildPlan("sets", []string{"host-a"}, PlanStep{
			Name: "s", Rollback: &dsl.RollbackSpec{SnapshotPaths: paths},
			Approval: &dsl.ApprovalSpec{Approvers: approvers},
		})
		p.RiskFactors = factors
		return ComputeHash(p)
	}
	a := mk(
		[]RiskFactor{{Rule: "z", Points: 2, Detail: "z"}, {Rule: "a", Points: 1, Detail: "a"}},
		[]string{"bob", "alice"}, []string{"/b", "/a"},
	)
	b := mk(
		[]RiskFactor{{Rule: "a", Points: 1, Detail: "a"}, {Rule: "z", Points: 2, Detail: "z"}},
		[]string{"alice", "bob"}, []string{"/a", "/b"},
	)
	assert.Equal(t, a, b)
}

func TestComputeHashMultiBatchDeterministic(t *testing.T) {
	plan := buildPlanBatches("wf", []Batch{
		{Index: 0, Targets: []string{"h-a", "h-b"}, Steps: []PlanStep{
			{Name: "s", Module: "pkg", Action: "upgrade"},
		}, MaxConcurrency: 2},
		{Index: 1, Targets: []string{"h-c"}, Steps: []PlanStep{
			{Name: "s", Module: "pkg", Action: "upgrade"},
		}, MaxConcurrency: 1},
	})
	h1 := ComputeHash(plan)
	h2 := ComputeHash(plan)
	assert.Equal(t, h1, h2)
}

// TestComputeHashHexEncoding verifies the version and lowercase digest encoding.
func TestComputeHashHexEncoding(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	assertV2Hash(t, ComputeHash(plan))
}
