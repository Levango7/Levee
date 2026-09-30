package wiring

// rollback_grade_test.go — 回滚分级动作的接线契约。
//
// 关注点不是「分级算得对不对」（那是 rollback.Grader 自己的单测），而是三件事：
// ① 动作在正确的 grade 上被派发，success 不派发；② 没有传输层时降级为记日志而
// 不是报错或静默；③ 传输层失败要能冒出来（engine 决定怎么处理），但不能变成
// 进程崩溃——nil 结果是合法输入。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/rollback"
)

type fakeSink struct {
	grades    []rollback.RollbackGrade
	runIDs    []string
	summaries []string
	err       error
}

func (f *fakeSink) NotifyGrade(_ context.Context, grade rollback.RollbackGrade, runID, summary string) error {
	f.grades = append(f.grades, grade)
	f.runIDs = append(f.runIDs, runID)
	f.summaries = append(f.summaries, summary)
	return f.err
}

func failedResult() *rollback.RollbackResult {
	return &rollback.RollbackResult{
		RunID:                  "run-5",
		Success:                false,
		PartialRollback:        true,
		RequiredCompensations:  3,
		CompletedCompensations: 1,
		UnknownSideEffects:     2,
	}
}

func TestNewRollbackGraderDispatchesFailureActions(t *testing.T) {
	sink := &fakeSink{}
	g := newRollbackGrader(sink)

	grade, err := g.GradeAndAct(context.Background(), failedResult())
	require.NoError(t, err)
	assert.Equal(t, rollback.GradePartial, grade)
	require.Len(t, sink.grades, 1)
	assert.Equal(t, rollback.GradePartial, sink.grades[0])
	assert.Equal(t, "run-5", sink.runIDs[0], "the notification must name the run it is about")
	assert.Contains(t, sink.summaries[0], "rollback grade=partial")
}

func TestNewRollbackGraderSuccessDispatchesNothing(t *testing.T) {
	sink := &fakeSink{}
	g := newRollbackGrader(sink)

	grade, err := g.GradeAndAct(context.Background(), &rollback.RollbackResult{RunID: "run-6", Success: true})
	require.NoError(t, err)
	assert.Equal(t, rollback.GradeSuccess, grade)
	assert.Empty(t, sink.grades, "success is logged, never notified")
}

func TestGradeNotifierWithoutSinkIsNotAnError(t *testing.T) {
	// 今天的生产形态：没有传输层。降级必须是「记日志并放过」，否则每次
	// 部分回滚都会在 VerifyAndGrade 里冒出一个错误。
	err := gradeNotifier(nil)(context.Background(), rollback.GradeFailure, failedResult())
	assert.NoError(t, err)
}

func TestGradeNotifierPropagatesDeliveryFailure(t *testing.T) {
	boom := errors.New("webhook refused")
	sink := &fakeSink{err: boom}

	err := gradeNotifier(sink)(context.Background(), rollback.GradeFailure, failedResult())
	assert.ErrorIs(t, err, boom, "a refused delivery must be visible to the caller")
}

func TestGradeActionsTolerateNilResult(t *testing.T) {
	// Grader.Grade(nil) 是合法的，产出 failure。通知路径上的空指针会把
	// 「回滚失败」变成「回滚失败且进程挂了」。
	require.NotPanics(t, func() {
		assert.NoError(t, gradeNotifier(&fakeSink{})(context.Background(), rollback.GradeFailure, nil))
		assert.NoError(t, gradeEscalator(context.Background(), rollback.GradeFailure, nil))
		assert.NoError(t, gradeAuditor(context.Background(), rollback.GradeFailure, nil))
	})
	assert.Empty(t, (&fakeSink{}).runIDs)
}

func TestRunIDOfNilResultIsEmpty(t *testing.T) {
	assert.Empty(t, runIDOf(nil))
	assert.Equal(t, "run-7", runIDOf(&rollback.RollbackResult{RunID: "run-7"}))
	assert.Zero(t, compensationsOf(nil, true))
	assert.Zero(t, compensationsOf(nil, false))
	assert.Zero(t, unknownOf(nil))
}