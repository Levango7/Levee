package rollback

// post_verify_context_test.go — 钉住「回滚后验证脱离调用方取消、但有界」这条契约。
//
// 这条契约是被一次真实的设计反复推翻后定下来的：最早的实现在调用方 ctx 上跑
// 门禁，于是「取消触发的回滚」永远得到一条 ctx 取消失败——一个关于请求的结论被
// 当成了关于系统的结论。改成脱离 ctx 之后，缺少上界又成了新问题（无界的门禁
// 扇出），所以 WithVerifyTimeout 与脱离是一起落地的，缺一不可。
//
// 下面四组分别钉住：超时不旋钮时门禁真的会跑、脱离不等于无界、默认上界存在、
// 分级动作的派发同样不受调用方取消影响。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/verify"
)

// ctxProbeGate records whether its Check ran, and whether the context it was
// handed was already cancelled on entry. The two are different questions and
// the bug lived exactly in the gap between them: a gate can run and still be
// handed a dead context (the old behaviour recorded "context canceled" as the
// gate's verdict), or be skipped entirely.
type ctxProbeGate struct {
	ran          atomic.Int32
	sawCancelled atomic.Bool
	// block, when non-nil, is waited on (or on ctx.Done) to model a gate
	// that outlives its budget.
	block chan struct{}
}

func (g *ctxProbeGate) Name() string                    { return "probe" }
func (g *ctxProbeGate) Phase() verify.GatePhase        { return verify.PhasePostApply }
func (g *ctxProbeGate) Check(ctx context.Context, _ verify.GateInput) (verify.GateResult, error) {
	g.ran.Add(1)
	if ctx.Err() != nil {
		g.sawCancelled.Store(true)
	}
	if g.block != nil {
		select {
		case <-g.block:
		case <-ctx.Done():
			return verify.GateResult{Passed: false, Message: "gate aborted: " + ctx.Err().Error()}, nil
		}
	}
	return verify.GateResult{Passed: true, Message: "ok"}, nil
}

func verifierWith(t *testing.T, g verify.Gate, opts ...PostRollbackVerifierOption) *PostRollbackVerifier {
	t.Helper()
	gm := verify.NewGateManager()
	gm.Register(g)
	v, err := NewPostRollbackVerifier(gm, opts...)
	require.NoError(t, err)
	return v
}

// 取消已经发生（Ctrl-C / fencing / 上游 deadline），门禁仍必须真的执行，并且
// 拿到的不是一个已死的 ctx。旧实现在这里得到 sawCancelled=true。
func TestVerifyRunsGatesDespiteCallerCancellation(t *testing.T) {
	gate := &ctxProbeGate{}
	v := verifierWith(t, gate)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone before verification starts

	res := v.Verify(ctx, &RollbackResult{Success: true}, nil, mkInput())

	require.NotNil(t, res)
	assert.Equal(t, int32(1), gate.ran.Load(), "gate must still run after caller cancellation")
	assert.False(t, gate.sawCancelled.Load(), "gate must not be handed an already-cancelled context")
	assert.True(t, res.Success, "a real check that passed must not be recorded as a ctx-cancelled failure")
	assert.NoError(t, res.Error)
}

// 脱离不等于无界：上界到了就收工，且门禁确实是被 deadline 掐断的。
func TestVerifyTimeoutBoundsDetachedWork(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	gate := &ctxProbeGate{block: block}
	v := verifierWith(t, gate, WithVerifyTimeout(40*time.Millisecond))

	// Caller context already cancelled: only the verifier's own bound can
	// end this call, which is exactly what makes it a real bound.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	res := v.Verify(ctx, &RollbackResult{Success: true}, nil, mkInput())
	elapsed := time.Since(start)

	require.NotNil(t, res)
	assert.Less(t, elapsed, 5*time.Second, "verification must honour its own bound")
	assert.Equal(t, int32(1), gate.ran.Load())
	assert.False(t, res.Success, "a gate that could not finish is not a pass")
	require.NotNil(t, res.Error)
	assert.Contains(t, res.Error.Error(), "deadline", "the bound, not the caller, ended the check")
	assert.Equal(t, 40*time.Millisecond, v.VerifyTimeout())
}

// 上界有默认值，且非正值回落到默认——避免 WithVerifyTimeout(0) 悄悄变成无界。
func TestVerifyTimeoutDefaults(t *testing.T) {
	assert.Equal(t, DefaultVerifyTimeout, verifierWith(t, &ctxProbeGate{}).VerifyTimeout())
	assert.Equal(t, DefaultVerifyTimeout, verifierWith(t, &ctxProbeGate{}, WithVerifyTimeout(0)).VerifyTimeout())
	assert.Equal(t, DefaultVerifyTimeout, verifierWith(t, &ctxProbeGate{}, WithVerifyTimeout(-time.Second)).VerifyTimeout())
	assert.Equal(t, time.Second, verifierWith(t, &ctxProbeGate{}, WithVerifyTimeout(time.Second)).VerifyTimeout())
}

// 分级动作的派发与门禁同一条规则：调用方已取消时仍然要发出去，否则最该通知的
// 场景（取消触发的回滚失败）恰恰是通知不出去的场景。
func TestVerifyAndGradeDispatchesDespiteCallerCancellation(t *testing.T) {
	var notified atomic.Int32
	var gotRunID atomic.Value
	grader := NewGrader(WithFailureNotify(func(_ context.Context, _ RollbackGrade, res *RollbackResult) error {
		notified.Add(1)
		if res != nil {
			gotRunID.Store(res.RunID)
		}
		return nil
	}))

	// A failing gate + a failed rollback is the failure grade.
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("bad", verify.PhasePostApply, false))
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader), WithVerifyTimeout(time.Second))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, dispatchErr := v.VerifyAndGrade(ctx,
		&RollbackResult{RunID: "run-42", Success: false, PartialRollback: false}, nil, mkInput())

	require.NoError(t, dispatchErr)
	require.NotNil(t, res)
	assert.Equal(t, int32(1), notified.Load(), "notify must fire even when the caller is gone")
	assert.Equal(t, "run-42", gotRunID.Load())
	assert.Equal(t, GradeFailure, res.Grade)
}

// 派发失败要能被调用方看见（engine 决定怎么处理），但不能被吞掉。
func TestVerifyAndGradeReportsDispatchFailure(t *testing.T) {
	grader := NewGrader(WithPartialNotify(func(context.Context, RollbackGrade, *RollbackResult) error {
		return assert.AnError
	}))
	v := verifierWith(t, &ctxProbeGate{}, WithGrader(grader))

	res, dispatchErr := v.VerifyAndGrade(context.Background(),
		&RollbackResult{Success: false, PartialRollback: true}, nil, mkInput())

	require.Error(t, dispatchErr)
	assert.Contains(t, dispatchErr.Error(), "notify")
	require.NotNil(t, res, "the verification result is still returned alongside a dispatch error")
	assert.Equal(t, GradePartial, res.Grade)
}