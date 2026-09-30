package dsl

// rollback_scope_test.go — workflow 级 rollback 归属规则（LE097）的测试。
//
// 规则本身在 rollback_scope.go 内实现，validator 与 plan.Generator 共用；
// 这里覆盖规则的接受面（运行态策略）、拒绝面（补偿内容 / 未知 on_failure）
// 与代码字面量的一致性（LE097 vs internal/errors）。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/errors"
)

// TestCodeWorkflowRollbackScopeMatchesCatalogue pins the LE097 literal of
// this package against the catalogue in internal/errors. Both the validator
// and the plan generator report the code, so a one-sided edit would let the
// two layers disagree about the very same violation.
func TestCodeWorkflowRollbackScopeMatchesCatalogue(t *testing.T) {
	assert.Equal(t, errors.LE097, CodeWorkflowRollbackScope)
}

// TestValidateRunLevelRollbackAcceptsRunLevelPolicy: 运行态策略是 workflow
// 级唯一合法的内容，且整块与 on_failure 均可省略（缺省 auto）。
func TestValidateRunLevelRollbackAcceptsRunLevelPolicy(t *testing.T) {
	cases := []struct {
		name string
		spec *RollbackSpec
	}{
		{"nil block", nil},
		{"empty block", &RollbackSpec{}},
		{"on_failure auto", &RollbackSpec{OnFailure: RollbackOnFailureAuto}},
		{"on_failure manual", &RollbackSpec{OnFailure: RollbackOnFailureManual}},
		{"manual with verify_after", &RollbackSpec{OnFailure: RollbackOnFailureManual, VerifyAfter: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Empty(t, ValidateRunLevelRollback(tc.spec, "rollback"))
		})
	}
}

// TestValidateRunLevelRollbackRejectsCompensation: 补偿契约（策略 / 撤销
// 步骤 / snapshot 路径）必须落在 step 上，workflow 级声明无法归属到
// (host, 前向步骤) 对，因此逐项拒绝。
func TestValidateRunLevelRollbackRejectsCompensation(t *testing.T) {
	cases := []struct {
		name  string
		spec  *RollbackSpec
		field string
	}{
		{
			name:  "strategy",
			spec:  &RollbackSpec{Strategy: "snapshot"},
			field: "rollback.strategy",
		},
		{
			name:  "undo steps",
			spec:  &RollbackSpec{Steps: []Step{{Name: "undo", Module: "pkg", Action: "downgrade"}}},
			field: "rollback.steps",
		},
		{
			name:  "snapshot paths",
			spec:  &RollbackSpec{SnapshotPaths: []string{"/etc/app.conf"}},
			field: "rollback.snapshot_paths",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateRunLevelRollback(tc.spec, "rollback")
			require.Len(t, errs, 1)
			assert.Equal(t, CodeWorkflowRollbackScope, errs[0].Code)
			assert.Equal(t, tc.field, errs[0].Field)
			assert.Contains(t, errs[0].Message, "steps[].rollback",
				"the message must tell the author where the declaration belongs")
		})
	}
}

// TestValidateRunLevelRollbackRejectsUnknownOnFailure: on_failure 是有取值
// 集合的字段，拼写错误必须在编译期暴露，而不是在运行期被当成"非 manual"
// 静默放过。
func TestValidateRunLevelRollbackRejectsUnknownOnFailure(t *testing.T) {
	errs := ValidateRunLevelRollback(&RollbackSpec{OnFailure: "abort"}, "rollback")
	require.Len(t, errs, 1)
	assert.Equal(t, codeEnumIllegal, errs[0].Code)
	assert.Equal(t, "rollback.on_failure", errs[0].Field)
	assert.Contains(t, errs[0].Message, RollbackOnFailureAuto)
	assert.Contains(t, errs[0].Message, RollbackOnFailureManual)
}

// TestValidateWorkflowRejectsWorkflowLevelCompensation drives the rule
// through a real document: a workflow-level undo list that used to be
// parsed, hashed and then silently ignored now fails validation. The parser
// layer still accepts the syntax (see TestParseRollbackNested) — the scope
// rule is the validator's, exactly where compile-time semantics belong.
func TestValidateWorkflowRejectsWorkflowLevelCompensation(t *testing.T) {
	yaml := `
name: workflow-level-undo
target:
  name: db
  hosts: [db1]
steps:
  - name: migrate
    action: mysql.exec
    args: {sql: "ALTER TABLE t ADD COLUMN c INT"}
rollback:
  strategy: undo-action
  on_failure: manual
  steps:
    - name: undo-migrate
      action: mysql.exec
      args: {sql: "ALTER TABLE t DROP COLUMN c"}
`
	wf, err := NewParser().ParseBytes([]byte(yaml))
	require.NoError(t, err)
	require.NotNil(t, wf.Rollback)
	require.Len(t, wf.Rollback.Steps, 1, "parser keeps the syntax layer permissive")

	errs := NewValidator().Validate(wf)
	var scopeErrs []ValidationError
	for _, e := range errs {
		if e.Code == CodeWorkflowRollbackScope {
			scopeErrs = append(scopeErrs, e)
		}
	}
	require.Len(t, scopeErrs, 2, "strategy + steps are both unattributable: %v", errs)
	assert.Equal(t, "rollback.strategy", scopeErrs[0].Field)
	assert.Equal(t, "rollback.steps", scopeErrs[1].Field)
}

// TestValidateWorkflowAcceptsStepLevelCompensation is the counterpart: the
// full compensation vocabulary (snapshot paths, undo steps) stays legal when
// each declaration sits on the step it belongs to, and the run-level policy
// stays legal at the workflow level.
func TestValidateWorkflowAcceptsStepLevelCompensation(t *testing.T) {
	yaml := `
name: step-level-rollback
target:
  name: web
  hosts: [web1]
steps:
  - name: push-conf
    action: file.copy
    args: {src: /srv/app.conf, dst: /etc/app.conf}
    rollback:
      strategy: snapshot
      snapshot_paths:
        - /etc/app.conf
  - name: restart
    action: svc.restart
    args: {unit: nginx}
    rollback:
      strategy: undo-action
      steps:
        - name: undo-restart
          action: svc.restart
          args: {unit: nginx}
rollback:
  on_failure: manual
  verify_after: true
`
	wf, err := NewParser().ParseBytes([]byte(yaml))
	require.NoError(t, err)
	assert.Empty(t, NewValidator().Validate(wf),
		"step-level compensation plus run-level policy is the legal shape")
}

// TestIsRollbackOnFailurePolicy / TestResolveRollbackOnFailure pin the
// vocabulary and the default resolution the engine relies on.
func TestIsRollbackOnFailurePolicy(t *testing.T) {
	assert.True(t, IsRollbackOnFailurePolicy(RollbackOnFailureAuto))
	assert.True(t, IsRollbackOnFailurePolicy(RollbackOnFailureManual))
	assert.False(t, IsRollbackOnFailurePolicy(""))
	assert.False(t, IsRollbackOnFailurePolicy("abort"))
}

func TestResolveRollbackOnFailure(t *testing.T) {
	cases := []struct {
		name string
		spec *RollbackSpec
		want string
	}{
		{"nil spec", nil, RollbackOnFailureAuto},
		{"absent", &RollbackSpec{}, RollbackOnFailureAuto},
		{"auto", &RollbackSpec{OnFailure: RollbackOnFailureAuto}, RollbackOnFailureAuto},
		{"manual", &RollbackSpec{OnFailure: RollbackOnFailureManual}, RollbackOnFailureManual},
		// 未知值只可能来自门禁上线前落库的旧 plan，保持历史行为（auto）。
		{"legacy unknown", &RollbackSpec{OnFailure: "abort"}, RollbackOnFailureAuto},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ResolveRollbackOnFailure(tc.spec))
		})
	}
}
