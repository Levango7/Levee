package dsl

// validator_test.go — Validator 单元测试。
//
// 测试覆盖：
//   - 合法 workflow 通过校验
//   - 缺 name / target / step
//   - 无效 input 类型 / approval level / batch strategy
//   - percent steps 超范围 / fixed steps 非正
//   - step 缺 name / action / action 非 module.action 形式
//   - target 缺 hosts 和 query
//   - 多个错误同时存在
//   - ValidateStrict 行为

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validWorkflow 返回一个通过全部基础校验的 Workflow，作为测试基线。
func validWorkflow() *Workflow {
	return &Workflow{
		Meta: WorkflowMeta{Name: "valid-wf"},
		Inputs: []InputParam{
			{Name: "pkg", Type: "string"},
			{Name: "wait", Type: "duration"},
		},
		Targets: []TargetGroup{
			{Name: "t", Type: "host", Query: "env=prod"},
		},
		Steps: []Step{
			{Name: "upgrade", Module: "pkg", Action: "upgrade"},
		},
		Approval: &ApprovalSpec{Level: "high"},
		Batches:  BatchConfig{Strategy: "percent", Steps: []int{1, 10, 50, 100}},
	}
}

// findErr 在 errs 中查找 Code 等于 code 的错误，返回是否找到及其索引。
func findErr(errs []ValidationError, code string) (int, bool) {
	for i, e := range errs {
		if e.Code == code {
			return i, true
		}
	}
	return -1, false
}

// TestValidateValidWorkflow 验证一个合法 workflow 通过校验。
func TestValidateValidWorkflow(t *testing.T) {
	v := NewValidator()
	errs := v.Validate(validWorkflow())
	assert.Empty(t, errs, "valid workflow should produce no errors")
}

// TestValidateValidWorkflowStrict 验证 ValidateStrict 对合法 workflow 返回 nil。
func TestValidateValidWorkflowStrict(t *testing.T) {
	v := NewValidator()
	require.NoError(t, v.ValidateStrict(validWorkflow()))
}

// TestValidateMissingName 验证缺 name 报 LE002。
func TestValidateMissingName(t *testing.T) {
	wf := validWorkflow()
	wf.Meta.Name = ""
	v := NewValidator()
	errs := v.Validate(wf)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE002", errs[0].Code)
	assert.Equal(t, "name", errs[0].Field)
}

// TestValidateMissingTarget 验证缺 target 报 LE092。
func TestValidateMissingTarget(t *testing.T) {
	wf := validWorkflow()
	wf.Targets = nil
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE092")
	require.True(t, ok, "should report LE092 for missing target")
}

// TestValidateMissingStep 验证缺 step 报 LE093。
func TestValidateMissingStep(t *testing.T) {
	wf := validWorkflow()
	wf.Steps = nil
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE093")
	require.True(t, ok, "should report LE093 for missing step")
}

// TestValidateInvalidInputType 验证无效 input 类型报 LE003。
func TestValidateInvalidInputType(t *testing.T) {
	wf := validWorkflow()
	wf.Inputs = []InputParam{
		{Name: "bad", Type: "float"},
	}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE003")
	require.True(t, ok, "should report LE003 for invalid input type")
	// 确认字段路径正确
	for _, e := range errs {
		if e.Code == "LE003" {
			assert.Equal(t, "input[0].type", e.Field)
			assert.Contains(t, e.Message, "float")
		}
	}
}

// TestValidateInputTypeAllowed 验证四种合法 input 类型都通过。
func TestValidateInputTypeAllowed(t *testing.T) {
	for _, typ := range []string{"string", "int", "duration", "bool"} {
		wf := validWorkflow()
		wf.Inputs = []InputParam{{Name: "p", Type: typ}}
		v := NewValidator()
		errs := v.Validate(wf)
		assert.Empty(t, errs, "input type %q should be allowed", typ)
	}
}

// TestValidateInvalidApprovalLevel 验证无效 approval level 报 LE044。
func TestValidateInvalidApprovalLevel(t *testing.T) {
	wf := validWorkflow()
	wf.Approval = &ApprovalSpec{Level: "super-high"}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE044")
	require.True(t, ok, "should report LE044 for invalid approval level")
}

// TestValidateApprovalLevelAllowed 验证三种合法 approval level 都通过。
func TestValidateApprovalLevelAllowed(t *testing.T) {
	for _, lvl := range []string{"standard", "high", "emergency"} {
		wf := validWorkflow()
		wf.Approval = &ApprovalSpec{Level: lvl}
		v := NewValidator()
		errs := v.Validate(wf)
		assert.Empty(t, errs, "approval level %q should be allowed", lvl)
	}
}

// TestValidateApprovalNil 验证 nil approval 合法（可选块）。
func TestValidateApprovalNil(t *testing.T) {
	wf := validWorkflow()
	wf.Approval = nil
	v := NewValidator()
	errs := v.Validate(wf)
	assert.Empty(t, errs, "nil approval should be allowed")
}

// TestValidateApprovalLevelEmpty 验证空 approval level 合法（缺省 standard）。
func TestValidateApprovalLevelEmpty(t *testing.T) {
	wf := validWorkflow()
	wf.Approval = &ApprovalSpec{Level: ""}
	v := NewValidator()
	errs := v.Validate(wf)
	assert.Empty(t, errs, "empty approval level should be allowed")
}

// TestValidateInvalidBatchStrategy 验证无效 batch strategy 报 LE034。
func TestValidateInvalidBatchStrategy(t *testing.T) {
	wf := validWorkflow()
	wf.Batches = BatchConfig{Strategy: "unknown", Steps: []int{1}}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE034")
	require.True(t, ok, "should report LE034 for invalid batch strategy")
}

// TestValidateBatchStrategyAllowed 验证三种合法 batch strategy 都通过。
func TestValidateBatchStrategyAllowed(t *testing.T) {
	t.Run("percent", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "percent", Steps: []int{1, 100}}
		v := NewValidator()
		assert.Empty(t, v.Validate(wf))
	})
	t.Run("fixed", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "fixed", Steps: []int{1, 5, 10}}
		v := NewValidator()
		assert.Empty(t, v.Validate(wf))
	})
	t.Run("serial", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "serial"}
		v := NewValidator()
		assert.Empty(t, v.Validate(wf))
	})
}

// TestValidateBatchStrategyEmpty 验证空 strategy 合法（batches 可选）。
func TestValidateBatchStrategyEmpty(t *testing.T) {
	wf := validWorkflow()
	wf.Batches = BatchConfig{}
	v := NewValidator()
	assert.Empty(t, v.Validate(wf))
}

// TestValidatePercentStepsOutOfRange 验证 percent steps 超范围报 LE031。
func TestValidatePercentStepsOutOfRange(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "percent", Steps: []int{0, 100}}
		v := NewValidator()
		errs := v.Validate(wf)
		_, ok := findErr(errs, "LE031")
		require.True(t, ok, "percent step 0 should be out of range")
	})
	t.Run("over_100", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "percent", Steps: []int{1, 101}}
		v := NewValidator()
		errs := v.Validate(wf)
		_, ok := findErr(errs, "LE031")
		require.True(t, ok, "percent step 101 should be out of range")
	})
	t.Run("boundary_1_and_100", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "percent", Steps: []int{1, 100}}
		v := NewValidator()
		assert.Empty(t, v.Validate(wf), "percent steps 1 and 100 are valid boundaries")
	})
}

// TestValidateFixedStepsNonPositive 验证 fixed steps 非正报 LE102。
func TestValidateFixedStepsNonPositive(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "fixed", Steps: []int{0, 5}}
		v := NewValidator()
		errs := v.Validate(wf)
		_, ok := findErr(errs, "LE102")
		require.True(t, ok, "fixed step 0 should be non-positive")
	})
	t.Run("negative", func(t *testing.T) {
		wf := validWorkflow()
		wf.Batches = BatchConfig{Strategy: "fixed", Steps: []int{-1, 5}}
		v := NewValidator()
		errs := v.Validate(wf)
		_, ok := findErr(errs, "LE102")
		require.True(t, ok, "fixed step -1 should be non-positive")
	})
}

// TestValidateStepMissingName 验证 step 缺 name 报 LE002。
func TestValidateStepMissingName(t *testing.T) {
	wf := validWorkflow()
	wf.Steps = []Step{{Module: "pkg", Action: "upgrade"}}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE002")
	require.True(t, ok, "should report LE002 for missing step name")
	for _, e := range errs {
		if e.Code == "LE002" && e.Field == "steps[0].name" {
			return
		}
	}
	t.Fatal("expected error on steps[0].name")
}

// TestValidateStepMissingAction 验证 step 缺 action 报 LE002。
func TestValidateStepMissingAction(t *testing.T) {
	wf := validWorkflow()
	wf.Steps = []Step{{Name: "s1"}}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE002")
	require.True(t, ok, "should report LE002 for missing step action")
	for _, e := range errs {
		if e.Code == "LE002" && e.Field == "steps[0].action" {
			return
		}
	}
	t.Fatal("expected error on steps[0].action")
}

// TestValidateStepActionNotModuleActionFormat 验证 action 不是 module.action
// 形式（无点号）报 LE101。
func TestValidateStepActionNotModuleActionFormat(t *testing.T) {
	wf := validWorkflow()
	// splitAction 对无点号的输入返回 Module="", Action=输入。
	// 模拟这种情况：Module 为空但 Action 非空。
	wf.Steps = []Step{{Name: "s1", Action: "nodothere"}}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE101")
	require.True(t, ok, "should report LE101 for action without module prefix")
}

// TestValidateTargetMissingHostsAndQuery 验证 target 缺 hosts 和 query 报 LE002。
func TestValidateTargetMissingHostsAndQuery(t *testing.T) {
	wf := validWorkflow()
	wf.Targets = []TargetGroup{{Name: "t", Type: "host"}}
	v := NewValidator()
	errs := v.Validate(wf)
	_, ok := findErr(errs, "LE002")
	require.True(t, ok, "should report LE002 for target without hosts and query")
	for _, e := range errs {
		if e.Code == "LE002" && e.Field == "target[0]" {
			return
		}
	}
	t.Fatal("expected error on target[0]")
}

// TestValidateTargetWithHosts 验证仅有 hosts 的 target 合法。
func TestValidateTargetWithHosts(t *testing.T) {
	wf := validWorkflow()
	wf.Targets = []TargetGroup{{Name: "t", Type: "host", Hosts: []string{"web-01"}}}
	v := NewValidator()
	assert.Empty(t, v.Validate(wf))
}

// TestValidateTargetWithQuery 验证仅有 query 的 target 合法。
func TestValidateTargetWithQuery(t *testing.T) {
	wf := validWorkflow()
	wf.Targets = []TargetGroup{{Name: "t", Type: "host", Query: "env=prod"}}
	v := NewValidator()
	assert.Empty(t, v.Validate(wf))
}

// TestValidateMultipleErrors 验证多个错误同时存在时不短路。
func TestValidateMultipleErrors(t *testing.T) {
	wf := &Workflow{
		Meta: WorkflowMeta{}, // 缺 name
		Inputs: []InputParam{
			{Name: "bad", Type: "float"}, // 无效 input 类型
		},
		Targets: nil, // 缺 target
		Steps: []Step{
			{Name: "", Module: "pkg", Action: "upgrade"}, // 缺 step name
			{Name: "s2"}, // 缺 action
		},
		Approval: &ApprovalSpec{Level: "super"},    // 无效 approval level
		Batches:  BatchConfig{Strategy: "unknown"}, // 无效 batch strategy
	}
	v := NewValidator()
	errs := v.Validate(wf)

	// 期望至少包含以下错误码：
	expectedCodes := []string{
		"LE002", // 缺 name
		"LE092", // 缺 target
		"LE003", // 无效 input 类型
		"LE002", // 缺 step name
		"LE002", // 缺 action
		"LE044", // 无效 approval level
		"LE034", // 无效 batch strategy
	}
	require.GreaterOrEqual(t, len(errs), len(expectedCodes),
		"should report all errors without short-circuit")

	codes := make(map[string]int)
	for _, e := range errs {
		codes[e.Code]++
	}
	assert.GreaterOrEqual(t, codes["LE002"], 3, "should have at least 3 LE002 errors")
	assert.GreaterOrEqual(t, codes["LE092"], 1, "should have LE092")
	assert.GreaterOrEqual(t, codes["LE003"], 1, "should have LE003")
	assert.GreaterOrEqual(t, codes["LE044"], 1, "should have LE044")
	assert.GreaterOrEqual(t, codes["LE034"], 1, "should have LE034")
}

// TestValidateNilWorkflow 验证 nil workflow 报错。
func TestValidateNilWorkflow(t *testing.T) {
	v := NewValidator()
	errs := v.Validate(nil)
	require.Len(t, errs, 1)
	assert.Equal(t, "LE002", errs[0].Code)
	assert.Equal(t, "workflow", errs[0].Field)
}

// TestValidateStrictReturnsFirstError 验证 ValidateStrict 返回第一个错误。
func TestValidateStrictReturnsFirstError(t *testing.T) {
	wf := &Workflow{
		Meta:    WorkflowMeta{}, // 缺 name
		Targets: nil,            // 缺 target
	}
	v := NewValidator()
	err := v.ValidateStrict(wf)
	require.Error(t, err)

	var ve ValidationError
	require.ErrorAs(t, err, &ve)
	assert.NotEmpty(t, ve.Code)
}

// TestValidateStrictNoErrors 验证 ValidateStrict 对合法 workflow 返回 nil。
func TestValidateStrictNoErrors(t *testing.T) {
	v := NewValidator()
	require.NoError(t, v.ValidateStrict(validWorkflow()))
}

// TestValidationErrorFormat 验证 ValidationError.Error() 格式。
func TestValidationErrorFormat(t *testing.T) {
	e := ValidationError{
		Code:    "LE002",
		Field:   "name",
		Message: "workflow name is required",
	}
	s := e.Error()
	assert.Contains(t, s, "LE002")
	assert.Contains(t, s, "workflow name is required")
	assert.Contains(t, s, "field=name")
}

// TestValidateFromParsedYAML 验证从解析器产出的合法 workflow 能通过 Validator。
// 这确保 Validator 与 Parser 的 AST 结构一致。
func TestValidateFromParsedYAML(t *testing.T) {
	p := NewParser()
	wf, err := p.ParseBytes([]byte(`
name: parsed-valid
target: {type: host, query: "env=prod"}
steps:
  - name: s1
    action: shell.exec
input:
  - name: pkg
    type: string
batches:
  strategy: percent
  steps: [1, 100]
approval:
  level: standard
`))
	require.NoError(t, err)
	require.NotNil(t, wf)

	v := NewValidator()
	errs := v.Validate(wf)
	assert.Empty(t, errs, "parsed valid workflow should pass validation")
}

// TestValidateStepsMultiple 验证多个 step 中部分错误不影响其他 step 校验。
func TestValidateStepsMultiple(t *testing.T) {
	wf := validWorkflow()
	wf.Steps = []Step{
		{Name: "s1", Module: "pkg", Action: "upgrade"}, // 合法
		{Name: "", Module: "shell", Action: "exec"},    // 缺 name
		{Name: "s3"}, // 缺 action
	}
	v := NewValidator()
	errs := v.Validate(wf)
	require.Len(t, errs, 2, "should report 2 step errors")
	// 确认错误指向正确的 step 索引
	fields := make(map[string]bool)
	for _, e := range errs {
		fields[e.Field] = true
	}
	assert.True(t, fields["steps[1].name"], "should flag steps[1].name")
	assert.True(t, fields["steps[2].action"], "should flag steps[2].action")
}

// TestValidatePercentStepsMultipleOutOfRange 验证多个 percent step 超范围
// 都被报告。
func TestValidatePercentStepsMultipleOutOfRange(t *testing.T) {
	wf := validWorkflow()
	wf.Batches = BatchConfig{Strategy: "percent", Steps: []int{0, 50, 101}}
	v := NewValidator()
	errs := v.Validate(wf)
	count := 0
	for _, e := range errs {
		if e.Code == "LE031" {
			count++
		}
	}
	assert.Equal(t, 2, count, "should report 2 out-of-range percent steps")
}
