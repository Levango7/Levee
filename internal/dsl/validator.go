package dsl

// validator.go — LEVEELang AST 基础校验器
//
// 校验内容：
//   - 必填字段：name 非空、至少一个 target、至少一个 step
//   - 类型基础：input.type ∈ {string,int,duration,bool}；
//     approval.level ∈ {standard,high,emergency}；
//     batches.strategy ∈ {percent,fixed,serial}
//   - 批次声明合法性：percent steps ∈ [1,100]；fixed steps > 0；serial 无需 steps
//   - step：name 与 action 非空；action 必须是 module.action 形式
//   - target：静态 target 必须有 hosts 或 query 至少一个
//
// Validate 返回所有错误（不短路）；ValidateStrict 遇到第一个错误即返回。
//
// 错误码沿用 internal/errors 中定义的 LE001-LE096，自定义校验错误码
// 在本文件内以常量形式声明，避免与 errors 包耦合。

import (
	"fmt"
)

// 自定义校验错误码。当 internal/errors 中已有合适码时优先复用，
// 否则使用本文件内的扩展码（前缀 LE1xx，避免与既有 LE0xx 冲突）。
const (
	// codeRequiredField 复用 LE002：必需字段缺失。
	codeRequiredField = "LE002"
	// codeEnumIllegal 复用 LE003：枚举值非法。
	codeEnumIllegal = "LE003"
	// codeBatchStrategy 复用 LE034：strategy 与 steps 类型不匹配。
	codeBatchStrategy = "LE034"
	// codePercentRange 复用 LE031：百分比数组非法。
	codePercentRange = "LE031"
	// codeApprovalLevel 复用 LE044：审批级别非法。
	codeApprovalLevel = "LE044"
	// codeMissingTarget 复用 LE092：缺少 target 块。
	codeMissingTarget = "LE092"
	// codeMissingStep 复用 LE093：缺少 step 块。
	codeMissingStep = "LE093"
	// codeActionFormat 扩展码：action 不是 module.action 形式。
	codeActionFormat = "LE101"
	// codeFixedStepNonPositive 扩展码：fixed strategy 的 step 非正。
	codeFixedStepNonPositive = "LE102"
)

// allowedInputTypes 列出 input 参数允许的类型集合。
var allowedInputTypes = map[string]struct{}{
	"string":   {},
	"int":      {},
	"duration": {},
	"bool":     {},
}

// allowedApprovalLevels 列出 approval.level 允许的取值。
var allowedApprovalLevels = map[string]struct{}{
	"standard":  {},
	"high":      {},
	"emergency": {},
}

// allowedBatchStrategies 列出 batches.strategy 允许的取值。
// 任务要求 percent/fixed/serial 三种。
var allowedBatchStrategies = map[string]struct{}{
	"percent": {},
	"fixed":   {},
	"serial":  {},
}

// ValidationError 描述一条校验失败。Code 是稳定错误码（如 LE002），
// Field 是字段路径（如 "steps[0].action"），Message 是人类可读说明。
type ValidationError struct {
	Code    string
	Field   string
	Message string
}

// Error 返回单行文本表示，便于日志与 CLI 输出。
func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s (field=%s)", e.Code, e.Message, e.Field)
}

// Validator 对解析后的 Workflow AST 执行基础校验。零值不可用，
// 请通过 NewValidator 构造。Validator 无内部状态，可并发使用。
type Validator struct{}

// NewValidator 返回一个就绪的 Validator。
func NewValidator() *Validator {
	return &Validator{}
}

// Validate 对 wf 执行全部基础校验，返回所有发现的错误（不短路）。
// 当且仅当返回切片为空时 wf 通过校验。nil wf 视为一处错误。
func (v *Validator) Validate(wf *Workflow) []ValidationError {
	if wf == nil {
		return []ValidationError{{
			Code:    codeRequiredField,
			Field:   "workflow",
			Message: "workflow is nil",
		}}
	}

	var errs []ValidationError

	// 1. 必填字段：name 非空。
	if wf.Meta.Name == "" {
		errs = append(errs, ValidationError{
			Code:    codeRequiredField,
			Field:   "name",
			Message: "workflow name is required",
		})
	}

	// 2. 必填字段：至少一个 target。
	if len(wf.Targets) == 0 {
		errs = append(errs, ValidationError{
			Code:    codeMissingTarget,
			Field:   "target",
			Message: "at least one target is required",
		})
	}

	// 3. 必填字段：至少一个 step。
	if len(wf.Steps) == 0 {
		errs = append(errs, ValidationError{
			Code:    codeMissingStep,
			Field:   "steps",
			Message: "at least one step is required",
		})
	}

	// 4. input 类型校验。
	errs = append(errs, v.validateInputs(wf.Inputs)...)

	// 5. target 校验：静态 target 必须有 hosts 或 query 至少一个。
	errs = append(errs, v.validateTargets(wf.Targets)...)

	// 6. step 校验：name/action 非空、action 必须是 module.action 形式。
	errs = append(errs, v.validateSteps(wf.Steps)...)

	// 7. approval level 校验。
	errs = append(errs, v.validateApproval(wf.Approval)...)

	// 8. batches 校验：strategy 枚举 + steps 合法性。
	errs = append(errs, v.validateBatches(wf.Batches)...)

	return errs
}

// ValidateStrict 执行与 Validate 相同的校验，但遇到第一个错误即返回。
// 返回 nil 表示通过校验。
func (v *Validator) ValidateStrict(wf *Workflow) error {
	errs := v.Validate(wf)
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// validateInputs 校验每个 input 参数的类型在允许集合内。
// 空 Type 视为合法（缺省类型由 V1 类型检查处理）。
func (v *Validator) validateInputs(inputs []InputParam) []ValidationError {
	var errs []ValidationError
	for i, p := range inputs {
		field := fmt.Sprintf("input[%d].type", i)
		if p.Type == "" {
			// 缺省类型不在本基础校验范围内。
			continue
		}
		if _, ok := allowedInputTypes[p.Type]; !ok {
			errs = append(errs, ValidationError{
				Code:    codeEnumIllegal,
				Field:   field,
				Message: fmt.Sprintf("invalid input type %q (allowed: string, int, duration, bool)", p.Type),
			})
		}
	}
	return errs
}

// validateTargets 校验每个 target 至少有 hosts 或 query 之一。
// 同时存在的 target 既无 hosts 也无 query 视为非法。
func (v *Validator) validateTargets(targets []TargetGroup) []ValidationError {
	var errs []ValidationError
	for i, t := range targets {
		field := fmt.Sprintf("target[%d]", i)
		if len(t.Hosts) == 0 && t.Query == "" {
			errs = append(errs, ValidationError{
				Code:    codeRequiredField,
				Field:   field,
				Message: "static target must have hosts or query at least one",
			})
		}
	}
	return errs
}

// validateSteps 校验每个 step 的 name 与 action 非空，且 action 是
// module.action 形式（包含一个点号，且点号前后均非空）。
func (v *Validator) validateSteps(steps []Step) []ValidationError {
	var errs []ValidationError
	for i, s := range steps {
		base := fmt.Sprintf("steps[%d]", i)
		if s.Name == "" {
			errs = append(errs, ValidationError{
				Code:    codeRequiredField,
				Field:   base + ".name",
				Message: "step name is required",
			})
		}
		// action 在 AST 中已被 splitAction 拆分为 Module 与 Action；
		// 这里以原始组合形式校验。Module 与 Action 同时为空表示 action 缺失。
		if s.Module == "" && s.Action == "" {
			errs = append(errs, ValidationError{
				Code:    codeRequiredField,
				Field:   base + ".action",
				Message: "step action is required",
			})
			continue
		}
		// action 必须是 module.action 形式：Module 与 Action 都非空。
		if s.Module == "" || s.Action == "" {
			errs = append(errs, ValidationError{
				Code:    codeActionFormat,
				Field:   base + ".action",
				Message: fmt.Sprintf("step action %q must be in module.action format", joinAction(s.Module, s.Action)),
			})
		}
	}
	return errs
}

// validateApproval 校验 approval.level 在允许集合内。nil approval 视为合法
// （approval 是可选块，缺省为 standard）。
func (v *Validator) validateApproval(a *ApprovalSpec) []ValidationError {
	if a == nil {
		return nil
	}
	// 空 level 视为缺省 standard，不报错。
	if a.Level == "" {
		return nil
	}
	var errs []ValidationError
	if _, ok := allowedApprovalLevels[a.Level]; !ok {
		errs = append(errs, ValidationError{
			Code:    codeApprovalLevel,
			Field:   "approval.level",
			Message: fmt.Sprintf("invalid approval level %q (allowed: standard, high, emergency)", a.Level),
		})
	}
	return errs
}

// validateBatches 校验 batches.strategy 枚举与 steps 合法性。
// 空 strategy 视为缺省（不校验）。
func (v *Validator) validateBatches(b BatchConfig) []ValidationError {
	if b.Strategy == "" {
		return nil
	}
	var errs []ValidationError

	// strategy 枚举校验。
	if _, ok := allowedBatchStrategies[b.Strategy]; !ok {
		errs = append(errs, ValidationError{
			Code:    codeBatchStrategy,
			Field:   "batches.strategy",
			Message: fmt.Sprintf("invalid batch strategy %q (allowed: percent, fixed, serial)", b.Strategy),
		})
		// strategy 非法时不再校验 steps 语义，避免误报。
		return errs
	}

	// 按 strategy 校验 steps 合法性。
	switch b.Strategy {
	case "percent":
		for i, step := range b.Steps {
			field := fmt.Sprintf("batches.steps[%d]", i)
			if step < 1 || step > 100 {
				errs = append(errs, ValidationError{
					Code:    codePercentRange,
					Field:   field,
					Message: fmt.Sprintf("percent step %d out of range [1, 100]", step),
				})
			}
		}
	case "fixed":
		for i, step := range b.Steps {
			field := fmt.Sprintf("batches.steps[%d]", i)
			if step <= 0 {
				errs = append(errs, ValidationError{
					Code:    codeFixedStepNonPositive,
					Field:   field,
					Message: fmt.Sprintf("fixed step %d must be positive", step),
				})
			}
		}
	case "serial":
		// serial strategy 不需要 steps，不校验。
	}

	return errs
}

// joinAction 将 Module 与 Action 拼回原始形式，仅用于错误信息。
func joinAction(module, action string) string {
	if module == "" {
		return action
	}
	if action == "" {
		return module
	}
	return module + "." + action
}
