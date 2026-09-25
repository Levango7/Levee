package engine

// rollback_policy_test.go — 运行态回滚策略（plan.Rollback.OnFailure，spec §7.1）
// 在闭包执行器上的行为测试。
//
// 策略语义：auto（缺省）在失败时触发自动补偿；manual 抑制自动补偿，失败
// 的 run 保留已应用的批次，等待操作员手动回滚（RollbackChange）。
// 门禁侧：workflow 级 rollback 只允许运行态策略，补偿内容由 LE097 拒绝
// （见 internal/dsl/rollback_scope.go 与 internal/plan/rollback_scope_test.go）。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/verify"
)

// TestClosureRunner_ManualOnFailureSuppressesAutoRollback: on_failure=manual
// 时 run 失败但不得派发任何补偿指令，结果标记 ManualRollbackRequired，
// 已应用的批次保持原状（由操作员决定是否回滚）。
func TestClosureRunner_ManualOnFailureSuppressesAutoRollback(t *testing.T) {
	store := newTestStore(t)
	postApplyGate := verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false)
	cr := newTestClosureRunner(t, store, postApplyGate)

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	p.Rollback = &dsl.RollbackSpec{OnFailure: dsl.RollbackOnFailureManual}
	exec := &mockExecutor{}

	result, err := cr.Run(context.Background(), p, exec.exec)
	require.Error(t, err)
	assert.Equal(t, PhaseFailed, result.Phase)
	assert.True(t, result.ManualRollbackRequired)
	assert.Nil(t, result.RollbackResult, "no compensation may run under on_failure=manual")
	assert.Contains(t, err.Error(), "on_failure=manual")

	// Both batches were applied; nothing was undone.
	assert.Equal(t, 2, exec.callsFor("upgrade"))
	assert.Equal(t, 0, exec.callsFor("downgrade"))
	// Locks are still released: the suppression covers compensation only.
	assertLocksReleased(t, store, "host-a", "host-b")
}

// TestClosureRunner_AutoOnFailureRollsBack: 缺省（空串）、显式 auto 与门禁
// 上线前落库的旧值（如 abort）都保持历史行为——自动回滚。
func TestClosureRunner_AutoOnFailureRollsBack(t *testing.T) {
	for _, policy := range []string{"", dsl.RollbackOnFailureAuto, "abort"} {
		t.Run("policy="+policy, func(t *testing.T) {
			store := newTestStore(t)
			postApplyGate := verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false)
			cr := newTestClosureRunner(t, store, postApplyGate)

			p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
			p.Rollback = &dsl.RollbackSpec{OnFailure: policy}
			exec := &mockExecutor{}

			result, err := cr.Run(context.Background(), p, exec.exec)
			require.NoError(t, err)
			assert.Equal(t, PhaseRolledBack, result.Phase)
			assert.False(t, result.ManualRollbackRequired)
			require.NotNil(t, result.RollbackResult)
			assert.True(t, result.RollbackResult.Success)
			assert.Equal(t, 2, exec.callsFor("upgrade"))
			assert.Equal(t, 2, exec.callsFor("downgrade"))
			assertLocksReleased(t, store, "host-a", "host-b")
		})
	}
}
