package plan

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashPlan builds a plan with one batch, the given targets and steps,
// then returns its hash. Convenience helper for hash tests.
func hashPlan(name string, targets []string, steps ...PlanStep) string {
	return ComputeHash(buildPlan(name, targets, steps...))
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

// TestComputeHashFormat verifies the hash is a 64-char lowercase hex
// string (SHA-256 digest).
func TestComputeHashFormat(t *testing.T) {
	plan := buildPlan("wf-fmt", []string{"host-a"})
	h := ComputeHash(plan)
	assert.Len(t, h, 64, "sha256 hex length")
	for _, c := range h {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"hash char %q must be lowercase hex", c)
	}
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
	assert.Len(t, h, 64)
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
	assert.Len(t, h1, 64)
}

// TestComputeHashMultiBatchDeterministic verifies that a multi-batch
// plan hashes deterministically across repeated calls.
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

// TestComputeHashHexEncoding verifies the hash is hex by checking it
// only contains hex chars and is lowercase.
func TestComputeHashHexEncoding(t *testing.T) {
	plan := buildPlan("wf", []string{"host-a"})
	h := ComputeHash(plan)
	assert.False(t, strings.ContainsAny(h, "GHijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ "))
}
