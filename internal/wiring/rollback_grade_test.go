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

	"github.com/nexus/levee/internal/notify"
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

// recordingNotifier captures what a NotificationManager actually delivers.
type recordingNotifier struct {
	msgs []notify.Message
}

func (r *recordingNotifier) Name() string { return "recording" }

func (r *recordingNotifier) Send(_ context.Context, msg notify.Message) error {
	r.msgs = append(r.msgs, msg)
	return nil
}

// TestNotifyRollbackSinkDeliversToNotifier is why the adapter exists in the
// tree at all: it is the difference between "notify is wired" and "notify is
// one line away from being wired". It is exercised here against a real
// NotificationManager so that the grade→entry-point mapping, the run id and
// the severity are pinned while nothing depends on an actual transport.
func TestNotifyRollbackSinkDeliversToNotifier(t *testing.T) {
	mgr := notify.NewNotificationManager()
	rec := &recordingNotifier{}
	require.NoError(t, mgr.Register(rec))

	sink := &notifyRollbackSink{
		notifier:  notify.NewRollbackNotifier(mgr),
		initiator: "alice",
		approver:  "bob",
		oncall:    "carol",
	}

	require.NoError(t, sink.NotifyGrade(context.Background(), rollback.GradeFailure, "run-9", "3/1 compensated"))
	require.Len(t, rec.msgs, 1, "a failure grade must produce exactly one notification")
	assert.Equal(t, "run-9", rec.msgs[0].RunID, "the message must name the run it is about")
	assert.Equal(t, notify.LevelCritical, rec.msgs[0].Level,
		"a failed rollback is the critical tier, not a warning")
	assert.Contains(t, rec.msgs[0].Body, "3/1 compensated")
	assert.Len(t, rec.msgs[0].Recipients, 3, "initiator + approver + oncall")

	require.NoError(t, sink.NotifyGrade(context.Background(), rollback.GradePartial, "run-9", "partial"))
	require.Len(t, rec.msgs, 2)
	assert.Equal(t, notify.LevelWarning, rec.msgs[1].Level)

	// Success has no notification by design: the sink must not invent one.
	require.NoError(t, sink.NotifyGrade(context.Background(), rollback.GradeSuccess, "run-9", "all good"))
	assert.Len(t, rec.msgs, 2, "success must not notify")
}
