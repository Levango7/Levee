package engine

// rollback_verify_after_test.go — 回滚后验证（T037）在闭包执行器上的接线契约。
//
// 契约有两半：① 装配了 PostRollbackVerifier 只构成「能力」，是否真的验证由
// plan 决定（plan.Rollback.VerifyAfter，spec §7.1，opt-in）；② 未声明的 plan
// 必须与「未注入 verifier」时逐字一致——这是 wiring 从 nil 改为注入 verifier
// 之后唯一的回归防线。
//
// 门禁侧（workflow 级 rollback 只允许 on_failure / verify_after，补偿内容由
// LE097 拒绝）见 internal/plan/rollback_scope_test.go。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/lock"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// newRunnerWithPostVerifier 与 newTestClosureRunner 同构，区别是把 postVerifier
// 真的注入进去，并且 apply 门禁与 post-rollback 门禁分属两个 GateManager——
// 否则「post-verify 跑了几个门禁」会被 apply 阶段的门禁数量污染。
func newRunnerWithPostVerifier(t *testing.T, store state.Store, applyGates []verify.Gate, pvGates []verify.Gate) *ClosureRunner {
	t.Helper()
	gm := verify.NewGateManager()
	for _, g := range applyGates {
		gm.Register(g)
	}
	pgm := verify.NewGateManager()
	for _, g := range pvGates {
		pgm.Register(g)
	}
	pv, err := rollback.NewPostRollbackVerifier(pgm)
	require.NoError(t, err)
	return NewClosureRunner(store, lock.NewLockManager(lock.NewLockStore(store), store), gm,
		rollback.NewManager(rollback.WithWhitelistAll()), batch.NewController(), pv)
}

// TestPostRollbackVerifyRequested pins the opt-in resolution itself, so the
// table test below can be read as behaviour rather than as a guess about the
// helper's nil handling.
func TestPostRollbackVerifyRequested(t *testing.T) {
	assert.False(t, postRollbackVerifyRequested(nil), "nil plan")
	assert.False(t, postRollbackVerifyRequested(&plan.Plan{}), "plan without a rollback block")
	assert.False(t, postRollbackVerifyRequested(&plan.Plan{Rollback: &dsl.RollbackSpec{}}),
		"omitted verify_after parses to false and must stay off")
	assert.False(t, postRollbackVerifyRequested(&plan.Plan{Rollback: &dsl.RollbackSpec{VerifyAfter: false}}),
		"explicit false")
	assert.True(t, postRollbackVerifyRequested(&plan.Plan{Rollback: &dsl.RollbackSpec{VerifyAfter: true}}),
		"explicit true is the only way in")
}

// TestClosureRunner_PostRollbackVerifyIsOptIn: 回滚一定会发生（post-apply 门禁
// 故意失败），唯一变量是 verify_after。只有显式 true 才允许出现
// PostVerifyResult；其余情形必须为 nil，即与 verifier 未注入时不可区分。
func TestClosureRunner_PostRollbackVerifyIsOptIn(t *testing.T) {
	cases := []struct {
		name     string
		rollback *dsl.RollbackSpec
		wantRun  bool
	}{
		{"no rollback block", nil, false},
		{"rollback block without verify_after", &dsl.RollbackSpec{OnFailure: dsl.RollbackOnFailureAuto}, false},
		{"verify_after false", &dsl.RollbackSpec{VerifyAfter: false}, false},
		{"verify_after true", &dsl.RollbackSpec{VerifyAfter: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			cr := newRunnerWithPostVerifier(t, store,
				[]verify.Gate{verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false)},
				[]verify.Gate{verify.NewNoopGate("post-rollback-health", verify.PhasePostApply, true)})

			p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
			p.Rollback = tc.rollback
			exec := &mockExecutor{}

			result, err := cr.Run(context.Background(), p, exec.exec)
			require.NoError(t, err)
			require.Equal(t, PhaseRolledBack, result.Phase, "the failing post-apply gate must trigger rollback")
			require.NotNil(t, result.RollbackResult)

			if !tc.wantRun {
				assert.Nil(t, result.PostVerifyResult,
					"verify_after not opted in: post-rollback gates may not run")
				return
			}
			require.NotNil(t, result.PostVerifyResult, "verify_after: true must run the post-rollback gates")
			assert.True(t, result.PostVerifyResult.Success)
			assert.Len(t, result.PostVerifyResult.GateResults, 1,
				"phase mode runs exactly the gates the verifier's manager holds")
			// Opting in must not disturb the compensation itself.
			assert.Equal(t, 2, exec.callsFor("downgrade"))
			assertLocksReleased(t, store, "host-a", "host-b")
		})
	}
}
