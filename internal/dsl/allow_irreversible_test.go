package dsl

// allow_irreversible_test.go — V14 不可逆白名单（LE082）的端到端单元测试：
// 解析 → 校验 → 发射回环 → IR 携带。判定语义（显式声明优先、引擎固有
// 破坏性词表其次）与 executor.IrreversibleChecker.Check 的优先级对齐，
// 词表单一来源在本包的 DefaultIrreversibleActions / IsInherentIrreversible
// ——编译期与 plan 期对「天生不可逆」的认定不可能分叉。

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allowIrreversibleYAML 是带 V14 白名单的最小合法 workflow 文档。
const allowIrreversibleYAML = `
name: irr-gated
version: "1.0"
target:
  name: t
  type: static
  hosts: [10.0.0.1]
allow_irreversible:
  - mysql.replica_switch
steps:
  - name: switch
    action: mysql.replica_switch
    args:
      confirm: "yes"
    irreversible: true
`

// TestParseAllowIrreversible 验证 workflow 级字段被解析进 AST；缺省时保持 nil。
func TestParseAllowIrreversible(t *testing.T) {
	wf, err := NewParser().ParseBytes([]byte(allowIrreversibleYAML))
	require.NoError(t, err)
	assert.Equal(t, []string{"mysql.replica_switch"}, wf.AllowIrreversible)

	without, err := NewParser().ParseBytes([]byte(`
name: no-whitelist
version: "1.0"
target:
  name: t
  type: static
  hosts: [10.0.0.1]
steps:
  - name: s
    action: shell.exec
    args:
      cmd: echo hi
`))
	require.NoError(t, err)
	assert.Nil(t, without.AllowIrreversible, "absent whitelist must stay nil")
}

// TestValidateIrreversibleWhitelist_Gate 钉住 V14 的缺省拒绝语义：
// 判定为不可逆的步骤必须列名，白名单缺席 = 什么都没授权。
func TestValidateIrreversibleWhitelist_Gate(t *testing.T) {
	v := NewValidator()

	// 显式声明 + 已列名：通过。
	wf, err := NewParser().ParseBytes([]byte(allowIrreversibleYAML))
	require.NoError(t, err)
	assert.Empty(t, v.Validate(wf))

	// 同一文档抹掉白名单：LE082，指向具体步骤与动作。
	wf.AllowIrreversible = nil
	errs := v.Validate(wf)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE082", errs[0].Code)
	assert.Equal(t, "steps[0].action", errs[0].Field)
	assert.Contains(t, errs[0].Message, "mysql.replica_switch")

	// 固有破坏性词表命中（作者未声明 irreversible）同样要列名。
	inherent := validWorkflow()
	inherent.Steps = []Step{{Name: "wipe", Module: "pkg", Action: "remove"}}
	errs = v.Validate(inherent)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE082", errs[0].Code)
	assert.Contains(t, errs[0].Message, "pkg.remove")

	// 列名后通过。
	inherent.AllowIrreversible = []string{"pkg.remove"}
	assert.Empty(t, v.Validate(inherent))

	// 列了别的动作不等于授权本动作。
	inherent.AllowIrreversible = []string{"file.delete"}
	errs = v.Validate(inherent)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE082", errs[0].Code)

	// 可逆步骤不受门禁影响（validWorkflow 基线 = pkg.upgrade，无白名单也通过）。
	assert.Empty(t, v.Validate(validWorkflow()))
}

// TestValidateIrreversibleWhitelist_EntryFormat 钉住白名单条目本身的格式门：
// 不是 module.action 形式的条目按 LE101 拒绝（与 step action 同码）。
func TestValidateIrreversibleWhitelist_EntryFormat(t *testing.T) {
	v := NewValidator()

	for _, entry := range []string{"mysql", ".action", "module.", ""} {
		wf := validWorkflow()
		wf.AllowIrreversible = []string{entry}
		errs := v.Validate(wf)
		require.Len(t, errs, 1, "entry %q", entry)
		assert.Equal(t, "LE101", errs[0].Code, "entry %q", entry)
		assert.Equal(t, "allow_irreversible[0]", errs[0].Field, "entry %q", entry)
	}

	// 合法条目不报格式错。
	wf := validWorkflow()
	wf.AllowIrreversible = []string{"pkg.upgrade"}
	assert.Empty(t, v.Validate(wf))
}

// TestValidateIrreversibleWhitelist_MixedSteps 钉住多步骤混合时只报未列名者，
// 已列名的步骤不被误伤。
func TestValidateIrreversibleWhitelist_MixedSteps(t *testing.T) {
	v := NewValidator()
	wf := validWorkflow()
	wf.Steps = []Step{
		{Name: "ok", Module: "pkg", Action: "remove"},
		{Name: "bad", Module: "file", Action: "delete"},
		{Name: "plain", Module: "pkg", Action: "upgrade"},
	}
	wf.AllowIrreversible = []string{"pkg.remove"}
	errs := v.Validate(wf)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE082", errs[0].Code)
	assert.Equal(t, "steps[1].action", errs[0].Field)
	assert.Contains(t, errs[0].Message, "file.delete")
}

// TestEmitRoundTripsAllowIrreversible 钉住发射器契约：白名单排序输出、
// 字节稳定、发射结果可被解析器原样读回。
func TestEmitRoundTripsAllowIrreversible(t *testing.T) {
	wf := &Workflow{
		Meta:              WorkflowMeta{Name: "emit-irr"},
		Targets:           []TargetGroup{{Name: "t", Hosts: []string{"h1"}}},
		AllowIrreversible: []string{"b.two", "a.one"},
		Steps:             []Step{{Name: "s", Module: "a", Action: "one", Irreversible: true}},
	}

	data, err := MarshalWorkflow(wf)
	require.NoError(t, err)
	again, err := MarshalWorkflow(wf)
	require.NoError(t, err)
	assert.Equal(t, string(data), string(again), "emission must be byte-stable")

	back, err := NewParser().ParseBytes(data)
	require.NoError(t, err, "emitted workflow must parse")
	assert.Equal(t, []string{"a.one", "b.two"}, back.AllowIrreversible,
		"round trip must preserve the whitelist, sorted")
}

// TestIRCarriesAllowIrreversible 钉住 `levee compile --ir` 产物记录白名单：
// 编译产物本身就是「这次编译授权了什么」的凭证。
func TestIRCarriesAllowIrreversible(t *testing.T) {
	wf, err := NewParser().ParseBytes([]byte(allowIrreversibleYAML))
	require.NoError(t, err)

	ir, err := GenerateIR(wf, NewTypeRegistry())
	require.NoError(t, err)
	assert.Equal(t, []string{"mysql.replica_switch"}, ir.Workflow.AllowIrreversible)
}
