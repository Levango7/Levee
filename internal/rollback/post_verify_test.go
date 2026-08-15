package rollback

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/verify"
)

// --- test fixtures ---------------------------------------------------------

// newVerifier returns a PostRollbackVerifier with a fresh GateManager.
func newVerifier(t *testing.T, opts ...PostRollbackVerifierOption) *PostRollbackVerifier {
	t.Helper()
	gm := verify.NewGateManager()
	v, err := NewPostRollbackVerifier(gm, opts...)
	require.NoError(t, err)
	return v
}

// mkInput returns a minimal GateInput for tests.
func mkInput() verify.GateInput {
	return verify.GateInput{
		RunID:     "run-1",
		TargetIDs: []string{"host-a"},
	}
}

// --- construction ----------------------------------------------------------

func TestNewPostRollbackVerifierNilGateMgr(t *testing.T) {
	_, err := NewPostRollbackVerifier(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gate manager is nil")
}

func TestPostRollbackVerifierGateManagerAccessor(t *testing.T) {
	gm := verify.NewGateManager()
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)
	assert.Equal(t, gm, v.GateManager())
}

func TestWithGraderOption(t *testing.T) {
	grader := NewGrader()
	v := newVerifier(t, WithGrader(grader))
	assert.Equal(t, grader, v.grader)
}

// --- phase mode (empty verifyGates) ----------------------------------------

func TestVerifyPhaseModeNoGates(t *testing.T) {
	v := newVerifier(t)
	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "no gates → success")
	assert.Empty(t, res.FailedGates)
	assert.Empty(t, res.SkippedGates)
}

func TestVerifyPhaseModeAllPass(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	gm.Register(verify.NewNoopGate("slo", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Empty(t, res.FailedGates)
	assert.Len(t, res.GateResults, 2)
}

func TestVerifyPhaseModeOneFails(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	gm.Register(verify.NewNoopGate("slo", verify.PhasePostApply, false))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Contains(t, res.FailedGates, "slo")
	require.Error(t, res.Error)
	assert.Contains(t, res.Error.Error(), "slo")
}

func TestVerifyPhaseModeAllFail(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("a", verify.PhasePostApply, false))
	gm.Register(verify.NewNoopGate("b", verify.PhasePostApply, false))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	// RunPhase short-circuits on first failure, so only one gate is in
	// FailedGates; the other is skipped.
	assert.NotEmpty(t, res.FailedGates)
}

// --- named mode (non-empty verifyGates) ------------------------------------

func TestVerifyNamedModeAllPass(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	gm.Register(verify.NewNoopGate("slo", verify.PhasePostApply, true))
	gm.Register(verify.NewNoopGate("extra", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	// Only run health and slo, not extra.
	res := v.Verify(context.Background(), &RollbackResult{Success: true},
		[]string{"health", "slo"}, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Empty(t, res.FailedGates)
	assert.Len(t, res.GateResults, 2)
}

func TestVerifyNamedModeOneFails(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	gm.Register(verify.NewNoopGate("slo", verify.PhasePostApply, false))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true},
		[]string{"health", "slo"}, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Contains(t, res.FailedGates, "slo")
	assert.NotContains(t, res.FailedGates, "health")
}

func TestVerifyNamedModeUnknownGateSkipped(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true},
		[]string{"health", "nonexistent"}, mkInput())
	require.NotNil(t, res)
	// The unknown gate is recorded as skipped (and as a failure because
	// its placeholder result has Passed == false).
	assert.False(t, res.Success)
	assert.Contains(t, res.SkippedGates, "nonexistent")
}

func TestVerifyNamedModeEmptyListFallsBackToPhase(t *testing.T) {
	// An empty verifyGates slice triggers phase mode. This is the same as
	// TestVerifyPhaseModeAllPass; here we verify the fallback explicitly.
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true},
		[]string{}, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Len(t, res.GateResults, 1)
}

// --- grading integration ---------------------------------------------------

func TestVerifyWithGraderSuccess(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Equal(t, GradeSuccess, res.Grade)
}

func TestVerifyWithGraderVerifyFailureOverridesRollbackSuccess(t *testing.T) {
	// Rollback succeeded but post-verify fails → grade should be failure.
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, false))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Equal(t, GradeFailure, res.Grade, "verify failure should override rollback success")
}

func TestVerifyWithGraderRollbackPartialVerifyPass(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	partialResult := &RollbackResult{Success: false, PartialRollback: true, Error: errors.New("partial")}
	res := v.Verify(context.Background(), partialResult, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "verify passed")
	assert.Equal(t, GradePartial, res.Grade, "verify pass → grade reflects rollback partial")
}

func TestVerifyWithGraderRollbackFailureVerifyPass(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	failResult := &RollbackResult{Success: false, Error: errors.New("fail")}
	res := v.Verify(context.Background(), failResult, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "verify passed")
	assert.Equal(t, GradeFailure, res.Grade, "verify pass → grade reflects rollback failure")
}

func TestVerifyWithGraderNilRollbackResultVerifyPass(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), nil, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "verify passed")
	assert.Equal(t, GradeFailure, res.Grade, "nil rollback result → grade failure")
}

func TestVerifyWithoutGraderGradeEmpty(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	assert.Equal(t, RollbackGrade(""), res.Grade, "no grader → grade is empty")
}

// --- VerifyAndGrade --------------------------------------------------------

func TestVerifyAndGradeNoGrader(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res, dispatchErr := v.VerifyAndGrade(context.Background(),
		&RollbackResult{Success: true}, nil, mkInput())
	require.NoError(t, dispatchErr)
	assert.True(t, res.Success)
}

func TestVerifyAndGradeSuccessDispatchesNothing(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	notifyCalls := atomic.Int64{}
	grader := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalls.Add(1)
			return nil
		}),
	)
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res, dispatchErr := v.VerifyAndGrade(context.Background(),
		&RollbackResult{Success: true}, nil, mkInput())
	require.NoError(t, dispatchErr)
	assert.Equal(t, GradeSuccess, res.Grade)
	assert.Equal(t, int64(0), notifyCalls.Load(), "success should dispatch no actions")
}

func TestVerifyAndGradeVerifyFailureDispatchesFailureActions(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, false))
	notifyCalls := atomic.Int64{}
	escalateCalls := atomic.Int64{}
	auditCalls := atomic.Int64{}
	grader := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalls.Add(1)
			return nil
		}),
		WithFailureEscalate(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			escalateCalls.Add(1)
			return nil
		}),
		WithFailureAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			auditCalls.Add(1)
			return nil
		}),
	)
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res, dispatchErr := v.VerifyAndGrade(context.Background(),
		&RollbackResult{Success: true}, nil, mkInput())
	require.NoError(t, dispatchErr)
	assert.False(t, res.Success)
	assert.Equal(t, GradeFailure, res.Grade)
	assert.Equal(t, int64(1), notifyCalls.Load(), "failure should notify")
	assert.Equal(t, int64(1), escalateCalls.Load(), "failure should escalate")
	assert.Equal(t, int64(1), auditCalls.Load(), "failure should audit")
}

func TestVerifyAndGradePartialDispatchesPartialActions(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	notifyCalls := atomic.Int64{}
	auditCalls := atomic.Int64{}
	grader := NewGrader(
		WithPartialNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalls.Add(1)
			return nil
		}),
		WithPartialAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			auditCalls.Add(1)
			return nil
		}),
	)
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	partialResult := &RollbackResult{Success: false, PartialRollback: true, Error: errors.New("p")}
	res, dispatchErr := v.VerifyAndGrade(context.Background(), partialResult, nil, mkInput())
	require.NoError(t, dispatchErr)
	assert.True(t, res.Success, "verify passed")
	assert.Equal(t, GradePartial, res.Grade)
	assert.Equal(t, int64(1), notifyCalls.Load(), "partial should notify")
	assert.Equal(t, int64(1), auditCalls.Load(), "partial should audit")
}

func TestVerifyAndGradeNotifyErrorPropagates(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, false))
	wantErr := errors.New("notify broken")
	grader := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			return wantErr
		}),
	)
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res, dispatchErr := v.VerifyAndGrade(context.Background(),
		&RollbackResult{Success: true}, nil, mkInput())
	require.Error(t, dispatchErr)
	assert.Contains(t, dispatchErr.Error(), "post-verify grade notify")
	assert.ErrorIs(t, dispatchErr, wantErr)
	assert.False(t, res.Success)
}

// --- context cancellation --------------------------------------------------

func TestVerifyCancelledContext(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := v.Verify(ctx, &RollbackResult{Success: true}, nil, mkInput())
	require.NotNil(t, res)
	// A cancelled context causes the noop gate to return Passed == false.
	assert.False(t, res.Success)
}

// --- nil rollback result ---------------------------------------------------

func TestVerifyNilRollbackResultNoGrader(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), nil, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "nil rollback result with passing verify → success")
}

// --- gate error handling ---------------------------------------------------

// errorGate is a test gate that always returns an error from Check.
type errorGate struct {
	name string
}

func (g *errorGate) Name() string            { return g.name }
func (g *errorGate) Phase() verify.GatePhase { return verify.PhasePostApply }
func (g *errorGate) Check(_ context.Context, _ verify.GateInput) (verify.GateResult, error) {
	return verify.GateResult{}, errors.New("gate internal error")
}

func TestVerifyGateErrorTreatedAsFailure(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(&errorGate{name: "broken"})
	v, err := NewPostRollbackVerifier(gm)
	require.NoError(t, err)

	res := v.Verify(context.Background(), &RollbackResult{Success: true},
		[]string{"broken"}, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success, "gate returning error should be a failure")
	assert.Contains(t, res.FailedGates, "broken")
	require.Error(t, res.Error)
}

// --- integration with real rollback ----------------------------------------

func TestVerifyAfterRealRollbackSuccess(t *testing.T) {
	// Run a real rollback, then verify with passing gates.
	m := NewManager()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})
	rollbackRes := m.Rollback(context.Background(), p, newExecRecorder().fn())
	require.True(t, rollbackRes.Success)

	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), rollbackRes, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success)
	assert.Equal(t, GradeSuccess, res.Grade)
}

func TestVerifyAfterRealRollbackPartial(t *testing.T) {
	m := NewManager(WithStopOnError(false))
	rec := newExecRecorder()
	rec.setFail("pkg", "install", errors.New("boom"))
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
		withRollback(plan.PlanStep{Name: "s2", Module: "file", Action: "copy"},
			rbStep("undo-s2", "file", "delete")),
	})
	rollbackRes := m.Rollback(context.Background(), p, rec.fn())
	require.True(t, rollbackRes.PartialRollback)

	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, true))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), rollbackRes, nil, mkInput())
	require.NotNil(t, res)
	assert.True(t, res.Success, "verify passed")
	assert.Equal(t, GradePartial, res.Grade)
}

func TestVerifyAfterRealRollbackSuccessButVerifyFails(t *testing.T) {
	m := NewManager()
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})
	rollbackRes := m.Rollback(context.Background(), p, newExecRecorder().fn())
	require.True(t, rollbackRes.Success)

	// Post-verify fails: the system is not healthy even though rollback
	// succeeded.
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("health", verify.PhasePostApply, false))
	grader := NewGrader()
	v, err := NewPostRollbackVerifier(gm, WithGrader(grader))
	require.NoError(t, err)

	res := v.Verify(context.Background(), rollbackRes, nil, mkInput())
	require.NotNil(t, res)
	assert.False(t, res.Success)
	assert.Equal(t, GradeFailure, res.Grade, "verify failure overrides rollback success")
}
