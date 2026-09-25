package dsl

// rollback_scope.go — 工作流级（计划级）rollback 声明的归属规则（LE097）。
//
// LEVEELang 有两处 rollback 声明位置，语义严格分层（docs/leveelang-spec.md
// 第 7 章）：
//
//   - step 级 `steps[].rollback`：**补偿契约**。声明"这一步做完之后怎么撤销"。
//     回滚策略（snapshot / undo-action / config-revert）、撤销步骤
//     （steps[].rollback.steps）与 snapshot_paths 都落在这里。补偿账本按
//     (host, 前向步骤) 归属，只有 step 级声明可以被归因，因而只有 step 级
//     声明存在执行路径（rollback.Manager、snapshotter、快照能力门禁）。
//   - workflow 级 `rollback`：**运行态策略**。只允许 on_failure（auto /
//     manual）与 verify_after —— 它们描述"整次运行失败时怎么办"，与具体
//     主机、具体步骤无关。
//
// workflow 级声明 strategy / steps / snapshot_paths 是不可执行的：
//
//   - 撤销步骤无法归属到任何 (host, 前向步骤) 对。执行器只能要么对每个前向
//     步骤重放整份全局撤销清单（同一效果被撤销多次），要么整份跳过（漏撤销），
//     两者都不是作者声明的语义。
//   - snapshot_paths 在 workflow 级没有补偿基线可挂：恢复动作必须属于某个
//     前向步骤的补偿才有归属；逐 step 复制则会在后续步骤改写文件之后再次
//     采集，恢复出的不是 apply 前的原始基线。
//
// 这类声明此前被解析、写入 plan、计入 plan_hash，却没有任何执行路径读取
// —— 典型"声明了但什么都不做"。现在在两处门禁 fail-closed 拒绝（错误码
// LE097，见 internal/errors.LE097 与 spec 第 8 章）：
//
//   - dsl.Validator（`levee compile` 路径）；
//   - plan.Generator.Generate（服务端 plan 路径；wiring.GeneratePlan 解析后
//     直接生成 plan，不经过 dsl validator，因此生成器必须自己守这道门）。

import (
	"fmt"
	"strings"
)

// workflow 级 rollback.on_failure 的取值。
const (
	// RollbackOnFailureAuto triggers the automatic rollback when a run
	// fails: batch execution error, post-batch / post-apply gate failure,
	// or cancellation. It is the default when the field is absent.
	RollbackOnFailureAuto = "auto"

	// RollbackOnFailureManual suppresses the automatic rollback: a failed
	// run keeps its applied batches and waits for an operator to trigger
	// the rollback (RollbackChange / `levee rollback`). The engine reports
	// the suppressed state via ClosureResult.ManualRollbackRequired.
	RollbackOnFailureManual = "manual"

	// CodeWorkflowRollbackScope is the error code for unattributable
	// compensation content declared at workflow (plan) level. It mirrors
	// internal/errors.LE097; TestCodeWorkflowRollbackScopeMatchesCatalogue
	// pins the agreement so the two literals cannot drift apart.
	CodeWorkflowRollbackScope = "LE097"
)

// RollbackOnFailurePolicies lists every accepted rollback.on_failure value.
// It is the single source of truth for the vocabulary: the validation message
// and the engine's policy resolution both derive from it.
var RollbackOnFailurePolicies = []string{
	RollbackOnFailureAuto,
	RollbackOnFailureManual,
}

// IsRollbackOnFailurePolicy reports whether s is an accepted on_failure value.
func IsRollbackOnFailurePolicy(s string) bool {
	for _, known := range RollbackOnFailurePolicies {
		if s == known {
			return true
		}
	}
	return false
}

// ResolveRollbackOnFailure returns the effective run-level failure policy of a
// rollback declaration: auto when spec is nil, when on_failure is absent, or
// when it carries an unrecognised value — and manual only for an explicit
// "manual".
//
// Unrecognised values deliberately resolve to auto: the compile-time gates
// (dsl.Validator and plan.Generator) reject them, so an unknown value can only
// come from a plan stored before those gates existed. Keeping the historical
// behaviour there beats silently suppressing a rollback that the workflow
// never asked to suppress.
func ResolveRollbackOnFailure(spec *RollbackSpec) string {
	if spec != nil && spec.OnFailure == RollbackOnFailureManual {
		return RollbackOnFailureManual
	}
	return RollbackOnFailureAuto
}

// ValidateRunLevelRollback checks a workflow-level (plan-level) rollback
// declaration against the scope rule: run-level policy only. field is the
// declaration's path in the workflow document ("rollback") and prefixes every
// reported ValidationError.
//
// It reports one error per violation:
//
//   - strategy / steps / snapshot_paths at workflow level → LE097;
//   - on_failure outside RollbackOnFailurePolicies → LE003.
//
// A nil spec (no workflow-level rollback block) is valid: the block is
// optional, and so is on_failure inside it (absent means auto).
func ValidateRunLevelRollback(spec *RollbackSpec, field string) []ValidationError {
	if spec == nil {
		return nil
	}
	var errs []ValidationError

	if spec.Strategy != "" {
		errs = append(errs, ValidationError{
			Code:  CodeWorkflowRollbackScope,
			Field: field + ".strategy",
			Message: fmt.Sprintf(
				"workflow-level rollback strategy %q has no execution path: compensation is attributed per (host, forward step), so the strategy must be declared on the step it compensates (steps[].rollback.strategy)",
				spec.Strategy),
		})
	}
	if len(spec.Steps) > 0 {
		errs = append(errs, ValidationError{
			Code:  CodeWorkflowRollbackScope,
			Field: field + ".steps",
			Message: fmt.Sprintf(
				"workflow-level rollback declares %d undo step(s) that cannot be attributed to a forward step; declare them on the step they undo (steps[].rollback.steps)",
				len(spec.Steps)),
		})
	}
	if len(spec.SnapshotPaths) > 0 {
		errs = append(errs, ValidationError{
			Code:  CodeWorkflowRollbackScope,
			Field: field + ".snapshot_paths",
			Message: fmt.Sprintf(
				"workflow-level rollback declares %d snapshot path(s) with no compensating step to restore them for; declare them on the step whose pre-apply state they capture (steps[].rollback.snapshot_paths)",
				len(spec.SnapshotPaths)),
		})
	}
	if spec.OnFailure != "" && !IsRollbackOnFailurePolicy(spec.OnFailure) {
		errs = append(errs, ValidationError{
			Code:    codeEnumIllegal,
			Field:   field + ".on_failure",
			Message: fmt.Sprintf("invalid rollback on_failure %q (allowed: %s)", spec.OnFailure, strings.Join(RollbackOnFailurePolicies, ", ")),
		})
	}
	return errs
}
