package conversation

// engine_test.go exercises the conversation engine and session helpers.
// The suite targets full line coverage of the package; every public method
// and every state branch in HandleMessage is exercised at least once.
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/recommend"
)

// --- test helpers -----------------------------------------------------------

// newTestEngine builds a ConversationEngine with both recommend and diagnose
// engines wired up. The diagnose engine is constructed with an empty config
// so Diagnose returns quickly with a report that has Target set.
func newTestEngine() *ConversationEngine {
	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{})
	diag := diagnosis.NewDiagEngine(diagnosis.DiagEngineConfig{})
	return NewConversationEngine(ConversationEngineConfig{
		Recommend: rec,
		Diagnose:  diag,
	})
}

// newTestEngineNoDiagnose builds an engine with a recommend engine but no
// diagnose engine. Used to test the "诊断引擎未配置" path.
func newTestEngineNoDiagnose() *ConversationEngine {
	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{})
	return NewConversationEngine(ConversationEngineConfig{
		Recommend: rec,
	})
}

// newTestEngineNoRecommend builds an engine with neither engine. Used to
// test ErrNilRecommend.
func newTestEngineNoRecommend() *ConversationEngine {
	return NewConversationEngine(ConversationEngineConfig{})
}

// makeSession creates a session and asserts the creation succeeded.
func makeSession(t *testing.T, e *ConversationEngine, userID string) *Session {
	t.Helper()
	sess, err := e.NewSession(userID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	return sess
}

// --- NewConversationEngine --------------------------------------------------

func TestNewConversationEngine_Defaults(t *testing.T) {
	// nil logger and zero timeout should be replaced with defaults.
	e := NewConversationEngine(ConversationEngineConfig{})
	require.NotNil(t, e)
	assert.Equal(t, DefaultConversationTimeout, e.timeout)
	assert.NotNil(t, e.log)
	assert.NotNil(t, e.sessions)
	assert.Equal(t, 0, e.SessionCount())
}

func TestNewConversationEngine_WithConfig(t *testing.T) {
	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{})
	e := NewConversationEngine(ConversationEngineConfig{
		Recommend: rec,
		Timeout:   5 * time.Second,
	})
	require.NotNil(t, e)
	assert.Equal(t, 5*time.Second, e.timeout)
	assert.NotNil(t, e.recommend)
}

// --- Close ------------------------------------------------------------------

func TestConversationEngine_Close(t *testing.T) {
	e := newTestEngine()
	makeSession(t, e, "u1")
	makeSession(t, e, "u2")
	require.Equal(t, 2, e.SessionCount())

	err := e.Close()
	require.NoError(t, err)
	assert.Equal(t, 0, e.SessionCount())

	// Close is idempotent.
	err = e.Close()
	require.NoError(t, err)
}

// --- NewSession -------------------------------------------------------------

func TestConversationEngine_NewSession(t *testing.T) {
	e := newTestEngine()
	sess, err := e.NewSession("user-1")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "user-1", sess.UserID)
	assert.Equal(t, StateIdle, sess.GetState())
	assert.Empty(t, sess.AlertID)
	assert.False(t, sess.IsTerminal())
	assert.NotNil(t, sess.History())
	assert.Equal(t, 0, len(sess.History()))
}

func TestConversationEngine_NewSession_EmptyUser(t *testing.T) {
	e := newTestEngine()
	_, err := e.NewSession("")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyMessage))

	_, err = e.NewSession("   ")
	require.Error(t, err)
}

// --- NewSessionFromAlert ----------------------------------------------------

func TestConversationEngine_NewSessionFromAlert(t *testing.T) {
	e := newTestEngine()
	sess, err := e.NewSessionFromAlert("user-1", "alert-123")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "user-1", sess.UserID)
	assert.Equal(t, "alert-123", sess.AlertID)
	assert.Equal(t, StateDiagnosing, sess.GetState())
}

func TestConversationEngine_NewSessionFromAlert_EmptyArgs(t *testing.T) {
	e := newTestEngine()
	_, err := e.NewSessionFromAlert("", "alert-1")
	require.Error(t, err)

	_, err = e.NewSessionFromAlert("user-1", "")
	require.Error(t, err)
}

// --- GetSession -------------------------------------------------------------

func TestConversationEngine_GetSession(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	got, err := e.GetSession(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
}

func TestConversationEngine_GetSession_NotFound(t *testing.T) {
	e := newTestEngine()
	_, err := e.GetSession("nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound))
}

// --- CloseSession -----------------------------------------------------------

func TestConversationEngine_CloseSession(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	err := e.CloseSession(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, e.SessionCount())

	// Closing again should fail.
	err = e.CloseSession(sess.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestConversationEngine_CloseSession_NotFound(t *testing.T) {
	e := newTestEngine()
	err := e.CloseSession("nope")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound))
}

// --- ListSessions -----------------------------------------------------------

func TestConversationEngine_ListSessions(t *testing.T) {
	e := newTestEngine()
	makeSession(t, e, "u1")
	makeSession(t, e, "u1")
	makeSession(t, e, "u2")

	list := e.ListSessions("u1")
	assert.Len(t, list, 2)
	for _, s := range list {
		assert.Equal(t, "u1", s.UserID)
	}

	list = e.ListSessions("u2")
	assert.Len(t, list, 1)

	list = e.ListSessions("u3")
	assert.Empty(t, list)
}

// --- SessionCount -----------------------------------------------------------

func TestConversationEngine_SessionCount(t *testing.T) {
	e := newTestEngine()
	assert.Equal(t, 0, e.SessionCount())
	makeSession(t, e, "u1")
	assert.Equal(t, 1, e.SessionCount())
	makeSession(t, e, "u2")
	assert.Equal(t, 2, e.SessionCount())
}

// --- HandleMessage: errors --------------------------------------------------

func TestHandleMessage_EmptyMessage(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyMessage))

	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "   ")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrEmptyMessage))
}

func TestHandleMessage_SessionNotFound(t *testing.T) {
	e := newTestEngine()
	_, err := e.HandleMessage(context.Background(), "nope", "u1", "hello")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSessionNotFound))
}

func TestHandleMessage_UserMismatch(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u2", "hello")
	require.Error(t, err)
}

// --- HandleMessage: StateIdle -----------------------------------------------

func TestHandleMessage_Idle_Help(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/help")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Contains(t, reply.Text, "LEVEE 对话命令")
}

func TestHandleMessage_Idle_PlainText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "你好")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Contains(t, reply.Text, "已收到您的消息")
}

func TestHandleMessage_Idle_DiagnoseNoTarget(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Contains(t, reply.Text, "用法")
}

func TestHandleMessage_Idle_DiagnoseNoEngine(t *testing.T) {
	e := newTestEngineNoDiagnose()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Contains(t, reply.Text, "诊断引擎未配置")
	assert.Equal(t, StateDiagnosing, sess.GetState())
}

func TestHandleMessage_Idle_DiagnoseWithEngine(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	require.NotNil(t, reply)
	// After diagnose + recommend the session should be in Reviewing.
	assert.Equal(t, StateReviewing, sess.GetState())
	assert.NotNil(t, sess.GetRecommendation())
}

func TestHandleMessage_Idle_RecommendNoEngine(t *testing.T) {
	e := newTestEngineNoRecommend()
	sess := makeSession(t, e, "u1")

	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/recommend")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNilRecommend))
}

func TestHandleMessage_Idle_RecommendWithEngine(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/recommend")
	require.NoError(t, err)
	require.NotNil(t, reply)
}

// --- HandleMessage: StateReviewing ------------------------------------------

func TestHandleMessage_Reviewing_Approve(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	// Drive to Reviewing via /diagnose.
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	require.Equal(t, StateReviewing, sess.GetState())

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)
	require.NotNil(t, reply)
	assert.Equal(t, StateExecuting, sess.GetState())
	require.NotNil(t, reply.Action)
	assert.Equal(t, ActionApprove, reply.Action.Type)
}

func TestHandleMessage_Reviewing_ApproveEnglish(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "approve")
	require.NoError(t, err)
	assert.Equal(t, StateExecuting, sess.GetState())
	assert.Equal(t, ActionApprove, reply.Action.Type)
}

func TestHandleMessage_Reviewing_ApproveYes(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "yes")
	require.NoError(t, err)
	assert.Equal(t, StateExecuting, sess.GetState())
	assert.Equal(t, ActionApprove, reply.Action.Type)
}

func TestHandleMessage_Reviewing_Reject(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "拒绝")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	require.NotNil(t, reply.Action)
	assert.Equal(t, ActionReject, reply.Action.Type)
}

func TestHandleMessage_Reviewing_RejectEnglish(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "reject")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionReject, reply.Action.Type)
}

func TestHandleMessage_Reviewing_RejectNo(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "no")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionReject, reply.Action.Type)
}

func TestHandleMessage_Reviewing_Modify(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "修改")
	require.NoError(t, err)
	assert.Equal(t, StateReviewing, sess.GetState())
	require.NotNil(t, reply.Action)
	assert.Equal(t, ActionModify, reply.Action.Type)
}

func TestHandleMessage_Reviewing_ModifyEnglish(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "modify")
	require.NoError(t, err)
	assert.Equal(t, StateReviewing, sess.GetState())
	assert.Equal(t, ActionModify, reply.Action.Type)
}

func TestHandleMessage_Reviewing_OtherText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "这个方案安全吗？")
	require.NoError(t, err)
	assert.Equal(t, StateReviewing, sess.GetState())
	assert.Contains(t, reply.Text, "关于您的问题")
}

func TestHandleMessage_Reviewing_OtherTextNoRecommendation(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	// Force into Reviewing without a recommendation.
	sess.SetState(StateReviewing)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "有什么建议？")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "暂无建议")
}

// --- HandleMessage: StateExecuting ------------------------------------------

func TestHandleMessage_Executing_Cancel(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	// Drive to Executing via /diagnose + approve.
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)
	require.Equal(t, StateExecuting, sess.GetState())

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "取消")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	require.NotNil(t, reply.Action)
	assert.Equal(t, ActionCancel, reply.Action.Type)
}

func TestHandleMessage_Executing_CancelEnglish(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "cancel")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionCancel, reply.Action.Type)
}

func TestHandleMessage_Executing_CancelCommand(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/cancel")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionCancel, reply.Action.Type)
}

func TestHandleMessage_Executing_OtherText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "进度如何？")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "执行进行中")
}

// --- HandleMessage: terminal states ----------------------------------------

func TestHandleMessage_Terminal_Restart(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateDone)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/restart")
	require.NoError(t, err)
	assert.Equal(t, StateIdle, sess.GetState())
	assert.Contains(t, reply.Text, "会话已重置")
}

func TestHandleMessage_Terminal_OtherText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateDone)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "继续")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "会话已结束")
}

func TestHandleMessage_Failed_Restart(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateFailed)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/restart")
	require.NoError(t, err)
	assert.Equal(t, StateIdle, sess.GetState())
	assert.Contains(t, reply.Text, "会话已重置")
}

// --- HandleMessage: StateDiagnosing / StateRecommending ---------------------

func TestHandleMessage_Diagnosing_Cancel(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateDiagnosing)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "取消")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	require.NotNil(t, reply.Action)
	assert.Equal(t, ActionCancel, reply.Action.Type)
}

func TestHandleMessage_Diagnosing_OtherText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateDiagnosing)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "状态如何？")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "诊断进行中")
}

func TestHandleMessage_Recommending_Cancel(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateRecommending)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "cancel")
	require.NoError(t, err)
	assert.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionCancel, reply.Action.Type)
}

func TestHandleMessage_Recommending_OtherText(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateRecommending)

	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "好了吗？")
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "正在生成建议")
}

// --- Session helpers --------------------------------------------------------

func TestSession_AddMessage(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	msg := sess.AddMessage(RoleUser, "hello")
	assert.NotEmpty(t, msg.ID)
	assert.Equal(t, RoleUser, msg.Role)
	assert.Equal(t, "hello", msg.Content)
	assert.False(t, msg.Timestamp.IsZero())

	hist := sess.History()
	require.Len(t, hist, 1)
	assert.Equal(t, "hello", hist[0].Content)
}

func TestSession_AddMessageWithAction(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	action := &Action{Type: ActionApprove, Payload: map[string]string{"id": "r1"}}
	msg := sess.AddMessageWithAction(RoleSystem, "approved", action)
	assert.Equal(t, RoleSystem, msg.Role)
	require.NotNil(t, msg.Action)
	assert.Equal(t, ActionApprove, msg.Action.Type)
	assert.Equal(t, "r1", msg.Action.Payload["id"])
}

func TestSession_History_IsCopy(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.AddMessage(RoleUser, "a")
	sess.AddMessage(RoleUser, "b")

	hist := sess.History()
	require.Len(t, hist, 2)
	// Mutating the returned slice must not affect the session.
	hist[0].Content = "mutated"
	again := sess.History()
	assert.Equal(t, "a", again[0].Content)
}

func TestSession_SetState(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateExecuting)
	assert.Equal(t, StateExecuting, sess.GetState())
}

func TestSession_SetRecommendation(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	rec := &recommend.Recommendation{ID: "rec-1", Summary: "test"}
	sess.SetRecommendation(rec)
	got := sess.GetRecommendation()
	require.NotNil(t, got)
	assert.Equal(t, "rec-1", got.ID)
}

func TestSession_SetWorkflowID(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetWorkflowID("wf-1")
	// WorkflowID is stored; verify via the struct field (no getter to keep
	// the API small).
	assert.Equal(t, "wf-1", sess.WorkflowID)
}

func TestSession_IsTerminal(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	assert.False(t, sess.IsTerminal())
	sess.SetState(StateDone)
	assert.True(t, sess.IsTerminal())
	sess.SetState(StateFailed)
	assert.True(t, sess.IsTerminal())
	sess.SetState(StateIdle)
	assert.False(t, sess.IsTerminal())
}

func TestSession_Reset(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	sess.SetState(StateFailed)
	sess.SetRecommendation(&recommend.Recommendation{ID: "r"})
	sess.SetWorkflowID("wf")
	sess.AddMessage(RoleUser, "msg")

	sess.Reset()
	assert.Equal(t, StateIdle, sess.GetState())
	assert.Nil(t, sess.GetRecommendation())
	assert.Empty(t, sess.WorkflowID)
	// History is preserved.
	assert.Len(t, sess.History(), 1)
}

func TestSessionState_String(t *testing.T) {
	cases := []struct {
		state SessionState
		want  string
	}{
		{StateIdle, "idle"},
		{StateDiagnosing, "diagnosing"},
		{StateRecommending, "recommending"},
		{StateReviewing, "reviewing"},
		{StateExecuting, "executing"},
		{StateDone, "done"},
		{StateFailed, "failed"},
		{SessionState(99), "unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.state.String())
	}
}

// --- Concurrency -----------------------------------------------------------

func TestSession_ConcurrentAddMessage(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			sess.AddMessage(RoleUser, fmt.Sprintf("msg-%d", i))
		}(i)
	}
	wg.Wait()
	assert.Len(t, sess.History(), n)
}

func TestHandleMessage_Concurrent(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	const n = 50
	var wg sync.WaitGroup
	var ok int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "ping")
			if err == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(n), ok)
	// All messages should be recorded.
	assert.Len(t, sess.History(), n)
}

func TestConversationEngine_ConcurrentNewSession(t *testing.T) {
	e := newTestEngine()
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = e.NewSession("u1")
		}()
	}
	wg.Wait()
	assert.Equal(t, n, e.SessionCount())
}

// --- table-driven idle commands ---------------------------------------------

func TestHandleMessage_IdleCommands_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantSub string
	}{
		{"help", "/help", "LEVEE 对话命令"},
		{"plain", "hello", "已收到您的消息"},
		{"diagnose_no_arg", "/diagnose", "用法"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := newTestEngine()
			sess := makeSession(t, e, "u1")
			reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", c.text)
			require.NoError(t, err)
			require.NotNil(t, reply)
			assert.Contains(t, reply.Text, c.wantSub)
		})
	}
}

// --- table-driven reviewing replies -----------------------------------------

func TestHandleMessage_ReviewingReplies_TableDriven(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantState  SessionState
		wantAction ActionType
	}{
		{"approve_cn", "执行", StateExecuting, ActionApprove},
		{"approve_en", "approve", StateExecuting, ActionApprove},
		{"yes", "yes", StateExecuting, ActionApprove},
		{"y", "y", StateExecuting, ActionApprove},
		{"reject_cn", "拒绝", StateFailed, ActionReject},
		{"reject_en", "reject", StateFailed, ActionReject},
		{"no", "no", StateFailed, ActionReject},
		{"n", "n", StateFailed, ActionReject},
		{"modify_cn", "修改", StateReviewing, ActionModify},
		{"modify_en", "modify", StateReviewing, ActionModify},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := newTestEngine()
			sess := makeSession(t, e, "u1")
			_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
			require.NoError(t, err)
			require.Equal(t, StateReviewing, sess.GetState())

			reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", c.text)
			require.NoError(t, err)
			require.NotNil(t, reply)
			assert.Equal(t, c.wantState, sess.GetState())
			require.NotNil(t, reply.Action)
			assert.Equal(t, c.wantAction, reply.Action.Type)
		})
	}
}

// --- table-driven executing replies -----------------------------------------

func TestHandleMessage_ExecutingReplies_TableDriven(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantState  SessionState
		wantAction ActionType
		wantSub    string
	}{
		{"cancel_cn", "取消", StateFailed, ActionCancel, ""},
		{"cancel_en", "cancel", StateFailed, ActionCancel, ""},
		{"cancel_cmd", "/cancel", StateFailed, ActionCancel, ""},
		{"other", "status?", StateExecuting, ActionNone, "执行进行中"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := newTestEngine()
			sess := makeSession(t, e, "u1")
			_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
			require.NoError(t, err)
			_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
			require.NoError(t, err)
			require.Equal(t, StateExecuting, sess.GetState())

			reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", c.text)
			require.NoError(t, err)
			require.NotNil(t, reply)
			assert.Equal(t, c.wantState, sess.GetState())
			if c.wantSub != "" {
				assert.Contains(t, reply.Text, c.wantSub)
			} else {
				require.NotNil(t, reply.Action)
				assert.Equal(t, c.wantAction, reply.Action.Type)
			}
		})
	}
}

// --- table-driven terminal replies ------------------------------------------

func TestHandleMessage_TerminalReplies_TableDriven(t *testing.T) {
	cases := []struct {
		name      string
		state     SessionState
		text      string
		wantState SessionState
		wantSub   string
	}{
		{"done_restart", StateDone, "/restart", StateIdle, "会话已重置"},
		{"failed_restart", StateFailed, "/restart", StateIdle, "会话已重置"},
		{"done_other", StateDone, "more", StateDone, "会话已结束"},
		{"failed_other", StateFailed, "more", StateFailed, "会话已结束"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			e := newTestEngine()
			sess := makeSession(t, e, "u1")
			sess.SetState(c.state)

			reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", c.text)
			require.NoError(t, err)
			require.NotNil(t, reply)
			assert.Equal(t, c.wantState, sess.GetState())
			assert.Contains(t, reply.Text, c.wantSub)
		})
	}
}

// --- context handling -------------------------------------------------------

func TestHandleMessage_NilContext(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	// Should not panic with nil context.
	reply, err := e.HandleMessage(nil, sess.ID, "u1", "/help")
	require.NoError(t, err)
	require.NotNil(t, reply)
}

func TestHandleMessage_ContextTimeout(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	// Very short timeout; /help is fast so should still succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	reply, err := e.HandleMessage(ctx, sess.ID, "u1", "/help")
	require.NoError(t, err)
	require.NotNil(t, reply)
}

// --- helpers coverage -------------------------------------------------------

func TestNormalizeText(t *testing.T) {
	assert.Equal(t, "hello", normalizeText("  hello  "))
	assert.Equal(t, "", normalizeText("   "))
	assert.Equal(t, "a b c", normalizeText("a b c"))
}

func TestHasPrefix(t *testing.T) {
	assert.True(t, hasPrefix("/help", "/help"))
	assert.True(t, hasPrefix("/help me", "/help"))
	assert.False(t, hasPrefix("/helpful", "/help"))
	assert.False(t, hasPrefix("hello", "/help"))
}

func TestHelpText(t *testing.T) {
	s := helpText()
	assert.Contains(t, s, "/diagnose")
	assert.Contains(t, s, "/recommend")
	assert.Contains(t, s, "/help")
	assert.True(t, strings.Contains(s, "\n"))
}

func TestAnswerFromRecommendation_Nil(t *testing.T) {
	assert.Equal(t, "暂无建议", answerFromRecommendation(nil, ""))
}

func TestAnswerFromRecommendation_Summary(t *testing.T) {
	rec := &recommend.Recommendation{
		Summary:   "restart service",
		Approach:  "kubectl rollout restart",
		RiskLevel: recommend.RiskLow,
	}
	s := answerFromRecommendation(rec, "")
	assert.Contains(t, s, "restart service")
	assert.Contains(t, s, "kubectl rollout restart")
}

func TestAnswerFromRecommendation_Question(t *testing.T) {
	rec := &recommend.Recommendation{
		Summary:  "fix it",
		Approach: "do X",
	}
	s := answerFromRecommendation(rec, "why?")
	assert.Contains(t, s, "why?")
	assert.Contains(t, s, "fix it")
}

func TestBuildApprovalCard(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")
	rec := &recommend.Recommendation{
		ID:        "rec-1",
		Summary:   "test summary",
		Approach:  "test approach",
		RiskLevel: recommend.RiskMedium,
	}
	card := buildApprovalCard(sess, rec)
	require.NotNil(t, card)
	assert.Equal(t, "approval", string(card.Kind))
	assert.Contains(t, card.Title, "test summary")
}

// --- full flow integration --------------------------------------------------

func TestHandleMessage_FullFlow(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	// 1. diagnose -> reviewing
	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)
	require.NotNil(t, reply)
	require.Equal(t, StateReviewing, sess.GetState())

	// 2. approve -> executing
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "执行")
	require.NoError(t, err)
	require.Equal(t, StateExecuting, sess.GetState())

	// 3. cancel -> failed
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "取消")
	require.NoError(t, err)
	require.Equal(t, StateFailed, sess.GetState())

	// 4. restart -> idle
	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "/restart")
	require.NoError(t, err)
	require.Equal(t, StateIdle, sess.GetState())

	// History should have all the messages.
	hist := sess.History()
	assert.Greater(t, len(hist), 4)
}

func TestHandleMessage_FullFlow_Reject(t *testing.T) {
	e := newTestEngine()
	sess := makeSession(t, e, "u1")

	_, err := e.HandleMessage(context.Background(), sess.ID, "u1", "/diagnose localhost")
	require.NoError(t, err)

	_, err = e.HandleMessage(context.Background(), sess.ID, "u1", "拒绝")
	require.NoError(t, err)
	require.Equal(t, StateFailed, sess.GetState())
}

// --- NewSessionFromAlert flow -----------------------------------------------

func TestHandleMessage_FromAlertFlow(t *testing.T) {
	e := newTestEngine()
	sess, err := e.NewSessionFromAlert("u1", "alert-1")
	require.NoError(t, err)
	require.Equal(t, StateDiagnosing, sess.GetState())

	// While diagnosing, sending cancel should fail the session.
	reply, err := e.HandleMessage(context.Background(), sess.ID, "u1", "取消")
	require.NoError(t, err)
	require.Equal(t, StateFailed, sess.GetState())
	assert.Equal(t, ActionCancel, reply.Action.Type)
}
