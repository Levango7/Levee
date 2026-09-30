package plan

// run_snapshot_hash_test.go — run 级基线必须进 plan_hash。
//
// 理由很直接：基线决定「回滚会把什么还原」。两份只在基线上不同的计划**不是
// 同一份计划**——一份可能承诺还原 /etc/app.conf，另一份什么都不还原，而它们
// 的执行动作完全相同。哈希不覆盖它，就等于允许一份被批准的 artifact 在另一个
// 恢复面上执行。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
)

func planWithBaseline(spec *dsl.RunSnapshotSpec) *Plan {
	return &Plan{
		ID:           "plan-baseline",
		WorkflowName: "baseline-demo",
		Batches: []Batch{{
			Index:   0,
			Targets: []string{"host-a"},
			Steps: []PlanStep{{
				Name: "apply", Module: "file", Action: "copy",
			}},
			MaxConcurrency: 1,
		}},
		TotalTargets: 1,
		RunSnapshot:  spec,
	}
}

func TestRunSnapshotIsCoveredByHash(t *testing.T) {
	withBaseline := planWithBaseline(&dsl.RunSnapshotSpec{
		Scope: "run", Paths: []string{"/etc/app.conf"},
	})
	without := planWithBaseline(nil)

	require.NotEmpty(t, ComputeHash(withBaseline))
	assert.NotEqual(t, ComputeHash(withBaseline), ComputeHash(without),
		"a plan with a baseline and one without are different plans and must hash differently")

	// A different path set is a different restore surface.
	other := planWithBaseline(&dsl.RunSnapshotSpec{
		Scope: "run", Paths: []string{"/etc/other.conf"},
	})
	assert.NotEqual(t, ComputeHash(withBaseline), ComputeHash(other))
}

func TestRunSnapshotPathOrderDoesNotChangeHash(t *testing.T) {
	a := planWithBaseline(&dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/a", "/b"}})
	b := planWithBaseline(&dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/b", "/a"}})
	assert.Equal(t, ComputeHash(a), ComputeHash(b),
		"the declaration is a SET of paths; capture order is not part of what was approved")
}

func TestRunSnapshotOmittedTypeEqualsExplicitFile(t *testing.T) {
	omitted := planWithBaseline(&dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/a"}})
	explicit := planWithBaseline(&dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/a"}, Type: "file"})
	assert.Equal(t, ComputeHash(omitted), ComputeHash(explicit),
		"omitting the type and spelling it out declare the same thing")
}

func TestRunSnapshotHashRoundTrips(t *testing.T) {
	p := planWithBaseline(&dsl.RunSnapshotSpec{Scope: "run", Paths: []string{"/etc/app.conf"}, Type: "config"})
	h := ComputeHash(p)
	require.NotEmpty(t, h)
	assert.True(t, VerifyHash(p, h), "a plan carrying a baseline must still verify against its own hash")
}
