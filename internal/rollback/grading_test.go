package rollback

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
)

// --- Grade classification --------------------------------------------------

func TestGradeNilResult(t *testing.T) {
	g := NewGrader()
	grade := g.Grade(nil)
	assert.Equal(t, GradeFailure, grade, "nil result should grade as failure")
}

func TestGradeSuccess(t *testing.T) {
	g := NewGrader()
	res := &RollbackResult{Success: true}
	grade := g.Grade(res)
	assert.Equal(t, GradeSuccess, grade)
}

func TestGradePartial(t *testing.T) {
	g := NewGrader()
	res := &RollbackResult{
		Success:         false,
		PartialRollback: true,
		Error:           errors.New("some step failed"),
	}
	grade := g.Grade(res)
	assert.Equal(t, GradePartial, grade)
}

func TestGradeFailure(t *testing.T) {
	g := NewGrader()
	res := &RollbackResult{
		Success:         false,
		PartialRollback: false,
		Error:           errors.New("all steps failed"),
	}
	grade := g.Grade(res)
	assert.Equal(t, GradeFailure, grade)
}

func TestGradeSuccessWithSkippedOnly(t *testing.T) {
	// A rollback with only skipped steps (no RollbackSpec anywhere) is
	// considered successful: there was nothing to undo and no failure.
	g := NewGrader()
	res := &RollbackResult{Success: true}
	grade := g.Grade(res)
	assert.Equal(t, GradeSuccess, grade)
}

// --- GetAction mapping -----------------------------------------------------

func TestGetActionSuccess(t *testing.T) {
	g := NewGrader()
	action := g.GetAction(GradeSuccess)
	assert.Equal(t, GradeSuccess, action.Grade)
	assert.Nil(t, action.Notify, "success should not notify")
	assert.Nil(t, action.Escalate, "success should not escalate")
	assert.Nil(t, action.Audit, "success should not audit")
}

func TestGetActionPartial(t *testing.T) {
	notifyCalled := atomic.Bool{}
	auditCalled := atomic.Bool{}
	g := NewGrader(
		WithPartialNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalled.Store(true)
			return nil
		}),
		WithPartialAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			auditCalled.Store(true)
			return nil
		}),
	)

	action := g.GetAction(GradePartial)
	assert.Equal(t, GradePartial, action.Grade)
	require.NotNil(t, action.Notify)
	require.NotNil(t, action.Audit)
	assert.Nil(t, action.Escalate, "partial should not escalate")

	// Invoke to verify the wired callbacks fire.
	require.NoError(t, action.Notify(context.Background(), GradePartial, nil))
	require.NoError(t, action.Audit(context.Background(), GradePartial, nil))
	assert.True(t, notifyCalled.Load(), "partial notify callback should fire")
	assert.True(t, auditCalled.Load(), "partial audit callback should fire")
}

func TestGetActionFailure(t *testing.T) {
	g := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
		WithFailureEscalate(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
		WithFailureAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
	)

	action := g.GetAction(GradeFailure)
	assert.Equal(t, GradeFailure, action.Grade)
	assert.NotNil(t, action.Notify, "failure should notify")
	assert.NotNil(t, action.Escalate, "failure should escalate")
	assert.NotNil(t, action.Audit, "failure should audit")
}

func TestGetActionUnknownGrade(t *testing.T) {
	g := NewGrader()
	action := g.GetAction(RollbackGrade("bogus"))
	assert.Equal(t, RollbackGrade("bogus"), action.Grade)
	assert.Nil(t, action.Notify)
	assert.Nil(t, action.Escalate)
	assert.Nil(t, action.Audit)
}

// --- GetAction with no configured callbacks --------------------------------

func TestGetActionPartialNoCallbacks(t *testing.T) {
	g := NewGrader()
	action := g.GetAction(GradePartial)
	assert.Equal(t, GradePartial, action.Grade)
	assert.Nil(t, action.Notify, "no notify configured → nil")
	assert.Nil(t, action.Escalate)
	assert.Nil(t, action.Audit)
}

func TestGetActionFailureNoCallbacks(t *testing.T) {
	g := NewGrader()
	action := g.GetAction(GradeFailure)
	assert.Equal(t, GradeFailure, action.Grade)
	assert.Nil(t, action.Notify)
	assert.Nil(t, action.Escalate)
	assert.Nil(t, action.Audit)
}

// --- GradeAndAct -----------------------------------------------------------

func TestGradeAndActSuccess(t *testing.T) {
	notifyCalls := atomic.Int64{}
	g := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalls.Add(1)
			return nil
		}),
	)
	res := &RollbackResult{Success: true}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.NoError(t, err)
	assert.Equal(t, GradeSuccess, grade)
	assert.Equal(t, int64(0), notifyCalls.Load(), "success should dispatch no actions")
}

func TestGradeAndActPartialDispatches(t *testing.T) {
	notifyCalls := atomic.Int64{}
	auditCalls := atomic.Int64{}
	g := NewGrader(
		WithPartialNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			notifyCalls.Add(1)
			return nil
		}),
		WithPartialAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			auditCalls.Add(1)
			return nil
		}),
	)
	res := &RollbackResult{Success: false, PartialRollback: true, Error: errors.New("partial")}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.NoError(t, err)
	assert.Equal(t, GradePartial, grade)
	assert.Equal(t, int64(1), notifyCalls.Load(), "partial should notify")
	assert.Equal(t, int64(1), auditCalls.Load(), "partial should audit")
}

func TestGradeAndActFailureDispatches(t *testing.T) {
	notifyCalls := atomic.Int64{}
	escalateCalls := atomic.Int64{}
	auditCalls := atomic.Int64{}
	g := NewGrader(
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
	res := &RollbackResult{Success: false, PartialRollback: false, Error: errors.New("fail")}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.NoError(t, err)
	assert.Equal(t, GradeFailure, grade)
	assert.Equal(t, int64(1), notifyCalls.Load(), "failure should notify")
	assert.Equal(t, int64(1), escalateCalls.Load(), "failure should escalate")
	assert.Equal(t, int64(1), auditCalls.Load(), "failure should audit")
}

func TestGradeAndActNilResult(t *testing.T) {
	g := NewGrader()
	grade, err := g.GradeAndAct(context.Background(), nil)
	require.NoError(t, err, "no callbacks configured → no error")
	assert.Equal(t, GradeFailure, grade)
}

func TestGradeAndActNotifyErrorShortCircuits(t *testing.T) {
	wantErr := errors.New("notify channel broken")
	escalateCalls := atomic.Int64{}
	g := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			return wantErr
		}),
		WithFailureEscalate(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			escalateCalls.Add(1)
			return nil
		}),
		WithFailureAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
	)
	res := &RollbackResult{Success: false, Error: errors.New("fail")}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.Error(t, err)
	assert.Equal(t, GradeFailure, grade)
	assert.Contains(t, err.Error(), "rollback grade notify")
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(0), escalateCalls.Load(), "escalate should not run after notify error")
}

func TestGradeAndActEscalateErrorShortCircuits(t *testing.T) {
	wantErr := errors.New("escalate failed")
	auditCalls := atomic.Int64{}
	g := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
		WithFailureEscalate(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			return wantErr
		}),
		WithFailureAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			auditCalls.Add(1)
			return nil
		}),
	)
	res := &RollbackResult{Success: false, Error: errors.New("fail")}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.Error(t, err)
	assert.Equal(t, GradeFailure, grade)
	assert.Contains(t, err.Error(), "rollback grade escalate")
	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, int64(0), auditCalls.Load(), "audit should not run after escalate error")
}

func TestGradeAndActAuditError(t *testing.T) {
	wantErr := errors.New("audit store down")
	g := NewGrader(
		WithFailureNotify(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
		WithFailureEscalate(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error { return nil }),
		WithFailureAudit(func(_ context.Context, _ RollbackGrade, _ *RollbackResult) error {
			return wantErr
		}),
	)
	res := &RollbackResult{Success: false, Error: errors.New("fail")}
	grade, err := g.GradeAndAct(context.Background(), res)
	require.Error(t, err)
	assert.Equal(t, GradeFailure, grade)
	assert.Contains(t, err.Error(), "rollback grade audit")
	assert.ErrorIs(t, err, wantErr)
}

// --- GradeSummary ----------------------------------------------------------

func TestGradeSummaryNilResult(t *testing.T) {
	s := GradeSummary(GradeFailure, nil)
	assert.Contains(t, s, "nil result")
	assert.Contains(t, s, "failure")
}

func TestGradeSummarySuccess(t *testing.T) {
	res := &RollbackResult{Success: true, Duration: 150 * time.Millisecond}
	s := GradeSummary(GradeSuccess, res)
	assert.Contains(t, s, "success")
	assert.Contains(t, s, "success=true")
}

func TestGradeSummaryPartial(t *testing.T) {
	res := &RollbackResult{Success: false, PartialRollback: true, Duration: 42 * time.Millisecond}
	s := GradeSummary(GradePartial, res)
	assert.Contains(t, s, "partial")
	assert.Contains(t, s, "partial=true")
}

// --- integration with real RollbackResult ----------------------------------

func TestGradeRealRollbackResultSuccess(t *testing.T) {
	// Build a real RollbackResult via the Manager so the grading logic
	// sees the actual shape produced by the rollback pipeline.
	m := NewManager(WithWhitelistAll())
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})
	res := m.Rollback(context.Background(), p, newExecRecorder().fn())
	require.True(t, res.Success)

	g := NewGrader()
	assert.Equal(t, GradeSuccess, g.Grade(res))
}

func TestGradeRealRollbackResultPartial(t *testing.T) {
	m := NewManager(WithStopOnError(false), WithWhitelistAll())
	rec := newExecRecorder()
	rec.setFail("pkg", "install", errors.New("boom"))
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
		withRollback(plan.PlanStep{Name: "s2", Module: "file", Action: "copy"},
			rbStep("undo-s2", "file", "delete")),
	})
	res := m.Rollback(context.Background(), p, rec.fn())
	require.False(t, res.Success)
	require.True(t, res.PartialRollback, "one step fails, one succeeds → partial")

	g := NewGrader()
	assert.Equal(t, GradePartial, g.Grade(res))
}

func TestGradeRealRollbackResultFailure(t *testing.T) {
	m := NewManager(WithWhitelistAll())
	rec := newExecRecorder()
	rec.setFail("pkg", "install", errors.New("boom"))
	p := mkPlan([]plan.PlanStep{
		withRollback(plan.PlanStep{Name: "s1", Module: "pkg", Action: "remove"},
			rbStep("undo-s1", "pkg", "install")),
	})
	res := m.Rollback(context.Background(), p, rec.fn())
	require.False(t, res.Success)
	require.False(t, res.PartialRollback, "all steps fail → not partial")

	g := NewGrader()
	assert.Equal(t, GradeFailure, g.Grade(res))
}
