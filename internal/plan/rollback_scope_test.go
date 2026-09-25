package plan

// rollback_scope_test.go — 服务端 plan 边界的 rollback 归属门禁测试。
//
// wiring.GeneratePlan 解析 workflow 文档后直接调用 Generator.Generate，
// 不经过 dsl.Validator。因此"workflow 级 rollback 只能承载运行态策略"这条
// 规则必须由生成器自己守（LE097），否则服务端会把无法执行的补偿声明写进
// plan 与 plan_hash。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/errors"
)

// TestGenerateRejectsWorkflowLevelRollbackCompensation covers each
// unattributable field: the generator must refuse the workflow, not persist
// a declaration that no execution path can honour.
func TestGenerateRejectsWorkflowLevelRollbackCompensation(t *testing.T) {
	cases := []struct {
		name string
		spec *dsl.RollbackSpec
	}{
		{"strategy", &dsl.RollbackSpec{Strategy: "snapshot"}},
		{"undo steps", &dsl.RollbackSpec{Steps: []dsl.Step{{Name: "undo", Module: "pkg", Action: "downgrade"}}}},
		{"snapshot paths", &dsl.RollbackSpec{SnapshotPaths: []string{"/etc/app.conf"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := makeWorkflow("wf-scope", dsl.BatchConfig{Strategy: "serial"},
				dsl.Step{Name: "upgrade", Module: "pkg", Action: "upgrade"})
			wf.Rollback = tc.spec

			p, err := NewGenerator().Generate(wf, []string{"host-a"})
			require.Error(t, err)
			assert.Nil(t, p, "no plan may be produced from an unattributable declaration")

			var le *errors.LEVEEError
			require.ErrorAs(t, err, &le)
			assert.Equal(t, errors.LE097, le.Code)
			assert.Equal(t, errors.Fatal, le.Severity)
			assert.Contains(t, le.Message, "wf-scope", "the message must name the workflow")
			assert.Contains(t, le.Message, "steps[].rollback",
				"the message must tell the author where the declaration belongs")
		})
	}
}

// TestGenerateAcceptsRunLevelRollbackPolicy pins the accepted shape:
// on_failure / verify_after survive into the plan artifact (and therefore
// into the approved hash and the engine's failure path).
func TestGenerateAcceptsRunLevelRollbackPolicy(t *testing.T) {
	wf := makeWorkflow("wf-policy", dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "upgrade", Module: "pkg", Action: "upgrade"})
	wf.Rollback = &dsl.RollbackSpec{OnFailure: dsl.RollbackOnFailureManual, VerifyAfter: true}

	p, err := NewGenerator().Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.NotNil(t, p.Rollback)
	assert.Equal(t, dsl.RollbackOnFailureManual, p.Rollback.OnFailure)
	assert.True(t, p.Rollback.VerifyAfter)
	assert.Empty(t, p.Rollback.Strategy)
	assert.Empty(t, p.Rollback.Steps)
	assert.NotEmpty(t, ComputeHash(p), "the policy is part of the hashed artifact")
}

// TestGenerateAcceptsStepLevelCompensation is the counterpart at the plan
// boundary: step-level compensation content is exactly what the generator
// must carry into each PlanStep.
func TestGenerateAcceptsStepLevelCompensation(t *testing.T) {
	stepRollback := &dsl.RollbackSpec{
		Strategy:      "snapshot",
		SnapshotPaths: []string{"/etc/app.conf"},
	}
	wf := makeWorkflow("wf-step-comp", dsl.BatchConfig{Strategy: "serial"},
		dsl.Step{Name: "push", Module: "file", Action: "copy", Rollback: stepRollback})

	p, err := NewGenerator().Generate(wf, []string{"host-a"})
	require.NoError(t, err)
	require.Len(t, p.Batches, 1)
	require.Len(t, p.Batches[0].Steps, 1)
	assert.Equal(t, stepRollback, p.Batches[0].Steps[0].Rollback)
	assert.Nil(t, p.Rollback, "no workflow-level block was declared")
}
