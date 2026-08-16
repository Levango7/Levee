package main

// cmd_converse_test.go exercises the `levee converse` sub-command and its
// helpers. The ConversationEngine is driven through its public API against
// an in-process RecommendEngine so no external services are required.
// Stdin/stdout are replaced with bytes.Buffer / strings.Reader so the
// interactive REPL can be tested deterministically.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/recommend"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test fixtures ---------------------------------------------------------

// newTestEngine returns a fresh ConversationEngine wired with a default
// RecommendEngine, suitable for tests.
func newTestEngine(t *testing.T) *conversation.ConversationEngine {
	t.Helper()
	rec := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{})
	return conversation.NewConversationEngine(conversation.ConversationEngineConfig{
		Recommend: rec,
	})
}

// resetConverseFlags restores the converse package-level flag variables to
// their defaults. Tests must call it (usually via defer) to avoid leaking
// state into sibling tests.
func resetConverseFlags() {
	converseInteractive = false
	converseSessionID = ""
	converseUserID = DefaultConverseUserID
	converseAlertID = ""
	converseHistory = false
	converseList = false
}

// --- 1. Command registration -----------------------------------------------

func TestConverseCmdRegistered(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	cmd := findSub("converse")
	require.NotNil(t, cmd, "converse subcommand should be registered")
	assert.Equal(t, "converse", cmd.Name())
	assert.Equal(t, "converse [message]", cmd.Use)
}

func TestConverseCmdFlags(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	cmd := findSub("converse")
	require.NotNil(t, cmd)
	for _, name := range []string{"interactive", "session", "user", "alert", "history", "list"} {
		require.NotNil(t, cmd.Flags().Lookup(name), "missing --%s flag", name)
	}
	// Short flag for --interactive.
	f := cmd.Flags().Lookup("interactive")
	require.NotNil(t, f)
	assert.Equal(t, "i", f.Shorthand)
}

func TestNewConverseCmdDirect(t *testing.T) {
	cmd := newConverseCmd()
	require.NotNil(t, cmd)
	assert.Equal(t, "converse [message]", cmd.Use)
	// Args validator allows 0 or 1 positional args.
	require.NoError(t, cmd.Args(cmd, nil))
	require.NoError(t, cmd.Args(cmd, []string{"hi"}))
	require.Error(t, cmd.Args(cmd, []string{"a", "b"}))
}

// --- 2. Sentinel errors ----------------------------------------------------

func TestConverseSentinelErrors(t *testing.T) {
	assert.NotNil(t, ErrConverseNoMessage)
	assert.NotNil(t, ErrConverseSessionLost)
	assert.Equal(t, "converse: no message provided", ErrConverseNoMessage.Error())
	assert.Equal(t, "converse: session lost", ErrConverseSessionLost.Error())
}

// --- 3. runConverseOnce: single-shot mode ----------------------------------

func TestRunConverseOnce_NewSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, "", "alice", "hello", &buf))
	out := buf.String()
	assert.Contains(t, out, "💡 LEVEE:")
	assert.Contains(t, out, "状态: idle")
	assert.Contains(t, out, "会话:")
}

func TestRunConverseOnce_ExistingSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("bob")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, sess.ID, "bob", "你好", &buf))
	out := buf.String()
	assert.Contains(t, out, sess.ID)
	assert.Contains(t, out, "💡 LEVEE:")
}

func TestRunConverseOnce_FromAlert(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseAlertID = "alert-001"
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, "", "alice", "查看告警", &buf))
	out := buf.String()
	assert.Contains(t, out, "💡 LEVEE:")
}

func TestRunConverseOnce_JSONMode(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	optJSON = true
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, "", "alice", "hello", &buf))
	var env map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	data, ok := env["data"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, data["session_id"])
	assert.Equal(t, "idle", data["state"])
	reply, ok := data["reply"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, reply["text"])
}

func TestRunConverseOnce_EmptyMessage(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	err := runConverseOnce(engine, "", "alice", "   ", &buf)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConverseNoMessage)
}

func TestRunConverseOnce_SessionNotFound(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	err := runConverseOnce(engine, "no-such-session", "alice", "hi", &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestRunConverseOnce_HandleMessageError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// Create a session owned by another user; HandleMessage will reject.
	sess, err := engine.NewSession("carol")
	require.NoError(t, err)
	var buf bytes.Buffer
	err = runConverseOnce(engine, sess.ID, "alice", "hi", &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not the owner")
}

// --- 4. runConverse interactive mode ---------------------------------------

func TestRunConverseInteractive_BasicFlow(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("hello\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	assert.Contains(t, s, "LEVEE 对话已启动")
	assert.Contains(t, s, "levee> ")
	assert.Contains(t, s, "💡 LEVEE:")
	assert.Contains(t, s, "再见！")
}

func TestRunConverseInteractive_QuitAlias(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/quit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "再见！")
}

func TestRunConverseInteractive_EOFExits(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// No newline at end -> EOF after reading "hello".
	in := strings.NewReader("hello")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "💡 LEVEE:")
}

func TestRunConverseInteractive_EmptyEOF(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "LEVEE 对话已启动")
}

func TestRunConverseInteractive_HelpCommand(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/help\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	assert.Contains(t, s, "可用命令")
	assert.Contains(t, s, "/exit")
	assert.Contains(t, s, "/help")
	assert.Contains(t, s, "/state")
	assert.Contains(t, s, "/history")
	assert.Contains(t, s, "/sessions")
	assert.Contains(t, s, "/new")
}

func TestRunConverseInteractive_StateCommand(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/state\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	assert.Contains(t, s, "会话 ID:")
	assert.Contains(t, s, "用户 ID:")
	assert.Contains(t, s, "状态:")
}

func TestRunConverseInteractive_HistoryCommand(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// Send a message first so history is non-empty, then /history.
	in := strings.NewReader("hello\n/history\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	assert.Contains(t, s, "历史")
}

func TestRunConverseInteractive_HistoryEmpty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/history\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "无历史消息")
}

func TestRunConverseInteractive_SessionsCommand(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/sessions\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	// At least the current session should be listed.
	assert.Contains(t, out.String(), "alice")
}

func TestRunConverseInteractive_NewCommand(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/new\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "新会话已创建")
}

func TestRunConverseInteractive_BlankLinesIgnored(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("\n\n   \n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	// Should still exit cleanly; only one prompt sequence expected.
	assert.Contains(t, out.String(), "再见！")
}

func TestRunConverseInteractive_ExistingSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	in := strings.NewReader("/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, sess.ID, "alice", in, &out))
	assert.Contains(t, out.String(), sess.ID)
}

func TestRunConverseInteractive_SessionNotFound(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/exit\n")
	var out bytes.Buffer
	err := runConverseInteractive(engine, "missing", "alice", in, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestRunConverseInteractive_FromAlert(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseAlertID = "alert-7"
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "LEVEE 对话已启动")
}

func TestRunConverseInteractive_HandleMessageErrorPrinted(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// Empty message after trim is rejected by the engine, but the loop trims
	// and skips empty lines, so we send a real message that the engine will
	// accept. Instead, force an error by using a non-owner session: the
	// interactive loop creates the session with userID "alice", so sending
	// any text succeeds. To exercise the error branch we override the
	// session's owner by creating one manually and passing its id while
	// keeping the userID different.
	sess, err := engine.NewSession("carol")
	require.NoError(t, err)
	in := strings.NewReader("hi\n/exit\n")
	var out bytes.Buffer
	err = runConverseInteractive(engine, sess.ID, "alice", in, &out)
	// ensureSession succeeds (session exists), but HandleMessage fails
	// because alice is not the owner. The loop prints the error and
	// continues, then /exit returns nil.
	require.NoError(t, err)
	assert.Contains(t, out.String(), "❌")
}

// --- 5. printReply ---------------------------------------------------------

func TestConversePrintReply_TextOnly(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	r := &conversation.Reply{Text: "ok"}
	require.NoError(t, printReply(&buf, r, "sess-1", conversation.StateIdle))
	out := buf.String()
	assert.Contains(t, out, "💡 LEVEE: ok")
	assert.Contains(t, out, "会话: sess-1 | 状态: idle")
	assert.NotContains(t, out, "[卡片")
	assert.NotContains(t, out, "[动作")
}

func TestConversePrintReply_WithCard(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	r := &conversation.Reply{
		Text: "请审批",
		Card: &chatops.Card{Kind: chatops.CardKindApproval, Title: "审批请求", Summary: "升级内核"},
	}
	require.NoError(t, printReply(&buf, r, "s", conversation.StateReviewing))
	out := buf.String()
	assert.Contains(t, out, "[卡片:")
	assert.Contains(t, out, "审批请求")
	assert.Contains(t, out, "升级内核")
}

func TestConversePrintReply_WithAction(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	r := &conversation.Reply{
		Text:   "已批准",
		Action: &conversation.Action{Type: conversation.ActionApprove},
	}
	require.NoError(t, printReply(&buf, r, "s", conversation.StateExecuting))
	out := buf.String()
	assert.Contains(t, out, "[动作: approve]")
}

func TestConversePrintReply_NilActionNoneOmitted(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	r := &conversation.Reply{
		Text:   "已收到",
		Action: &conversation.Action{Type: conversation.ActionNone},
	}
	require.NoError(t, printReply(&buf, r, "s", conversation.StateIdle))
	assert.NotContains(t, buf.String(), "[动作")
}

func TestConversePrintReply_NilReply(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	require.NoError(t, printReply(&buf, nil, "s", conversation.StateIdle))
	out := buf.String()
	assert.Contains(t, out, "<空回复>")
	assert.Contains(t, out, "状态: idle")
}

func TestConversePrintReply_JSON(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	optJSON = true
	var buf bytes.Buffer
	r := &conversation.Reply{Text: "hi"}
	require.NoError(t, printReplyJSON(&buf, "s1", conversation.StateIdle, r))
	var env map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	data := env["data"].(map[string]any)
	assert.Equal(t, "s1", data["session_id"])
	assert.Equal(t, "idle", data["state"])
	reply := data["reply"].(map[string]any)
	assert.Equal(t, "hi", reply["text"])
}

// --- 6. printSessionInfo ---------------------------------------------------

func TestConversePrintSessionInfo_Full(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSessionFromAlert("alice", "alert-1")
	require.NoError(t, err)
	sess.DiagnosisID = "diag-1"
	sess.WorkflowID = "wf-1"
	sess.AddMessage(conversation.RoleUser, "hi")
	var buf bytes.Buffer
	require.NoError(t, printSessionInfo(&buf, sess))
	out := buf.String()
	assert.Contains(t, out, sess.ID)
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "alert-1")
	assert.Contains(t, out, "diag-1")
	assert.Contains(t, out, "wf-1")
	assert.Contains(t, out, "消息数:     1")
}

func TestConversePrintSessionInfo_NilSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	err := printSessionInfo(&buf, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConverseSessionLost)
}

// --- 7. printSessionHistory ------------------------------------------------

func TestConversePrintSessionHistory_NonEmpty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sess.AddMessage(conversation.RoleUser, "hello")
	sess.AddMessage(conversation.RoleAssistant, "hi there")
	var buf bytes.Buffer
	require.NoError(t, printSessionHistory(&buf, sess))
	out := buf.String()
	assert.Contains(t, out, "历史")
	assert.Contains(t, out, "hello")
	assert.Contains(t, out, "hi there")
}

func TestConversePrintSessionHistory_Empty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, printSessionHistory(&buf, sess))
	assert.Contains(t, buf.String(), "无历史消息")
}

func TestConversePrintSessionHistory_NilSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	var buf bytes.Buffer
	err := printSessionHistory(&buf, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConverseSessionLost)
}

// --- 8. printSessionList ---------------------------------------------------

func TestConversePrintSessionList_Empty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	require.NoError(t, printSessionList(&buf, engine, "nobody"))
	assert.Contains(t, buf.String(), "无活跃会话")
}

func TestConversePrintSessionList_NonEmpty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	_, err := engine.NewSession("alice")
	require.NoError(t, err)
	_, err = engine.NewSession("alice")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, printSessionList(&buf, engine, "alice"))
	out := buf.String()
	assert.Contains(t, out, "alice")
	assert.Contains(t, out, "活跃会话")
}

// --- 9. runConverseList ----------------------------------------------------

func TestRunConverseList_Human(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	_, err := engine.NewSession("alice")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, runConverseList(engine, "alice", &buf))
	assert.Contains(t, buf.String(), "alice")
}

func TestRunConverseList_JSON(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	optJSON = true
	engine := newTestEngine(t)
	defer engine.Close()
	_, err := engine.NewSession("alice")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, runConverseList(engine, "alice", &buf))
	var env map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	data, ok := env["data"].([]any)
	require.True(t, ok)
	assert.Len(t, data, 1)
	meta := env["meta"].(map[string]any)
	assert.Equal(t, "alice", meta["user_id"])
	assert.Equal(t, float64(1), meta["count"])
}

func TestRunConverseList_JSONEmpty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	optJSON = true
	engine := newTestEngine(t)
	defer engine.Close()
	var buf bytes.Buffer
	require.NoError(t, runConverseList(engine, "nobody", &buf))
	var env map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	data := env["data"].([]any)
	assert.Empty(t, data)
}

// --- 10. printHelp ---------------------------------------------------------

func TestConversePrintHelp(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printHelp(&buf))
	out := buf.String()
	for _, want := range []string{"/exit", "/quit", "/help", "/state", "/history", "/sessions", "/new"} {
		assert.Contains(t, out, want)
	}
}

// --- 11. formatCard --------------------------------------------------------

func TestConverseFormatCard_Nil(t *testing.T) {
	assert.Equal(t, "", formatCard(nil))
}

func TestConverseFormatCard_TitleAndSummary(t *testing.T) {
	c := &chatops.Card{Title: "审批请求", Summary: "升级内核"}
	assert.Equal(t, "审批请求 - 升级内核", formatCard(c))
}

func TestConverseFormatCard_TitleOnly(t *testing.T) {
	c := &chatops.Card{Title: "仅标题"}
	assert.Equal(t, "仅标题", formatCard(c))
}

func TestConverseFormatCard_SummaryOnly(t *testing.T) {
	c := &chatops.Card{Summary: "仅摘要"}
	assert.Equal(t, "仅摘要", formatCard(c))
}

func TestConverseFormatCard_BothEmpty(t *testing.T) {
	c := &chatops.Card{Kind: chatops.CardKindStatus}
	assert.Equal(t, "status", formatCard(c))
}

// --- 12. runConverse dispatch via cobra -------------------------------------

func TestRunConverse_DispatchList(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseList = true
	converseUserID = "alice"
	err := runConverse(nil, nil)
	// No sessions -> human output is "无活跃会话"; should succeed.
	require.NoError(t, err)
}

func TestRunConverse_DispatchHistoryRequiresSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseHistory = true
	converseSessionID = ""
	err := runConverse(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--history requires --session")
}

func TestRunConverse_DispatchHistoryUnknownSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseHistory = true
	converseSessionID = "no-such"
	err := runConverse(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
}

func TestRunConverse_DispatchSingleNoMessage(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	err := runConverse(nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrConverseNoMessage)
}

func TestRunConverse_DispatchSingleWithMessage(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	out, err := captureStdout(func() error {
		return runConverse(nil, []string{"hello"})
	})
	require.NoError(t, err)
	assert.Contains(t, out, "💡 LEVEE:")
}

func TestRunConverse_DispatchJSON(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	out, err := captureStdout(func() error {
		optJSON = true
		return runConverse(nil, []string{"hello"})
	})
	require.NoError(t, err)
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data := env["data"].(map[string]any)
	assert.NotEmpty(t, data["session_id"])
}

func TestRunConverse_DispatchInteractive(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseInteractive = true
	// Replace converseStdin so the REPL exits immediately on /exit.
	origStdin := converseStdin
	converseStdin = strings.NewReader("/exit\n")
	defer func() { converseStdin = origStdin }()
	out, err := captureStdout(func() error {
		return runConverse(nil, nil)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "再见！")
}

func TestRunConverse_UserIDDefaultsWhenEmpty(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseUserID = "  "
	out, err := captureStdout(func() error {
		return runConverse(nil, []string{"hello"})
	})
	require.NoError(t, err)
	assert.Contains(t, out, "💡 LEVEE:")
}

// --- 13. ensureSession -----------------------------------------------------

func TestConverseEnsureSession_New(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := ensureSession(engine, "", "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "alice", sess.UserID)
}

func TestConverseEnsureSession_FromAlert(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseAlertID = "alert-9"
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := ensureSession(engine, "", "alice")
	require.NoError(t, err)
	assert.Equal(t, "alert-9", sess.AlertID)
	assert.Equal(t, conversation.StateDiagnosing, sess.GetState())
}

func TestConverseEnsureSession_Existing(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	orig, err := engine.NewSession("alice")
	require.NoError(t, err)
	sess, err := ensureSession(engine, orig.ID, "alice")
	require.NoError(t, err)
	assert.Equal(t, orig.ID, sess.ID)
}

func TestConverseEnsureSession_NotFound(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	_, err := ensureSession(engine, "missing", "alice")
	require.Error(t, err)
}

func TestConverseEnsureSession_NewSessionError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// Empty user id makes NewSession fail.
	_, err := ensureSession(engine, "", "  ")
	require.Error(t, err)
}

func TestConverseEnsureSession_NewFromAlertError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseAlertID = "alert-x"
	engine := newTestEngine(t)
	defer engine.Close()
	// Empty user id makes NewSessionFromAlert fail.
	_, err := ensureSession(engine, "", "  ")
	require.Error(t, err)
}

// --- 14. newConversationEngineForCLI ---------------------------------------

func TestConverseNewEngineForCLI(t *testing.T) {
	engine, err := newConversationEngineForCLI()
	require.NoError(t, err)
	require.NotNil(t, engine)
	assert.Equal(t, 0, engine.SessionCount())
	defer engine.Close()
}

// --- 15. handleInteractiveCommand ------------------------------------------

func TestConverseHandleInteractiveCommand_Exit(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sessionID := sess.ID
	var out bytes.Buffer
	handled, err := handleInteractiveCommand(&out, engine, &sess, &sessionID, "alice", "/exit")
	assert.True(t, handled)
	assert.ErrorIs(t, err, errConverseExit)
	assert.Contains(t, out.String(), "再见！")
}

func TestConverseHandleInteractiveCommand_Unknown(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sessionID := sess.ID
	var out bytes.Buffer
	handled, err := handleInteractiveCommand(&out, engine, &sess, &sessionID, "alice", "just text")
	assert.False(t, handled)
	require.NoError(t, err)
}

func TestConverseHandleInteractiveCommand_New(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	origID := sess.ID
	sessionID := sess.ID
	var out bytes.Buffer
	handled, err := handleInteractiveCommand(&out, engine, &sess, &sessionID, "alice", "/new")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Contains(t, out.String(), "新会话已创建")
	// Session pointer and id should have been updated to a new value.
	assert.NotEqual(t, origID, sessionID)
	assert.Equal(t, sessionID, sess.ID)
}

// --- 16. End-to-end via cobra ---------------------------------------------

func TestConverseViaRootCmd_JSON(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	out, err := executeCommand("converse", "hello", "--json")
	require.NoError(t, err)
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data := env["data"].(map[string]any)
	assert.NotEmpty(t, data["session_id"])
}

func TestConverseViaRootCmd_Human(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	out, err := executeCommand("converse", "hello")
	require.NoError(t, err)
	assert.Contains(t, out, "💡 LEVEE:")
}

func TestConverseViaRootCmd_NoArgs(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	_, err := executeCommand("converse")
	require.Error(t, err)
}

func TestConverseViaRootCmd_TooManyArgs(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	_, err := executeCommand("converse", "a", "b")
	require.Error(t, err)
}

// --- 17. DefaultConverseUserID constant -----------------------------------

func TestDefaultConverseUserID(t *testing.T) {
	assert.Equal(t, "cli-user", DefaultConverseUserID)
}

// --- 18. errConverseExit sentinel -----------------------------------------

func TestErrConverseExit(t *testing.T) {
	assert.NotNil(t, errConverseExit)
	assert.Equal(t, "converse: exit requested", errConverseExit.Error())
}

// --- 19. SessionState string coverage -------------------------------------

func TestConversePrintReply_AllStates(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	states := []conversation.SessionState{
		conversation.StateIdle,
		conversation.StateDiagnosing,
		conversation.StateRecommending,
		conversation.StateReviewing,
		conversation.StateExecuting,
		conversation.StateDone,
		conversation.StateFailed,
	}
	for _, st := range states {
		var buf bytes.Buffer
		r := &conversation.Reply{Text: "x"}
		require.NoError(t, printReply(&buf, r, "s", st))
		assert.Contains(t, buf.String(), st.String())
	}
}

// --- 20. Read error path ---------------------------------------------------

func TestRunConverseInteractive_ReadError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := &errorReader{}
	var out bytes.Buffer
	err := runConverseInteractive(engine, "", "alice", in, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read input")
}

// errorReader is an io.Reader that always returns a non-EOF error.
type errorReader struct{}

func (errorReader) Read(p []byte) (int, error) {
	return 0, errors.New("simulated read failure")
}

// --- 21. Context propagation ----------------------------------------------

func TestRunConverseOnce_UsesContext(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// We can't easily inject a context; just verify the call succeeds and
	// the engine records the message in history.
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, "", "alice", "ping", &buf))
	sessions := engine.ListSessions("alice")
	require.Len(t, sessions, 1)
	hist := sessions[0].History()
	// At least the user's message should be in history.
	var found bool
	for _, m := range hist {
		if m.Role == conversation.RoleUser && m.Content == "ping" {
			found = true
			break
		}
	}
	assert.True(t, found, "user message should be in history")
}

// --- 22. Multiple messages in interactive mode ----------------------------

func TestRunConverseInteractive_MultipleMessages(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("msg1\nmsg2\nmsg3\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	// Three replies should be present.
	assert.Equal(t, 3, strings.Count(s, "💡 LEVEE:"))
}

// --- 23. /sessions after /new shows both --------------------------------

func TestRunConverseInteractive_NewThenSessions(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/new\n/sessions\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	assert.Contains(t, s, "活跃会话")
}

// --- 24. Verify cobra command metadata ------------------------------------

func TestConverseCmdMetadata(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	cmd := findSub("converse")
	require.NotNil(t, cmd)
	assert.Equal(t, "与 LEVEE 对话引擎交互", cmd.Short)
	assert.Contains(t, cmd.Long, "单次模式")
	assert.Contains(t, cmd.Long, "交互模式")
	assert.Contains(t, cmd.Long, "指定会话")
}

// --- 25. Print no panic on long messages ---------------------------------

func TestConversePrintReply_LongMessage(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	long := strings.Repeat("abcdefghij", 1000)
	r := &conversation.Reply{Text: long}
	var buf bytes.Buffer
	require.NoError(t, printReply(&buf, r, "s", conversation.StateIdle))
	assert.Contains(t, buf.String(), long)
}

// --- 26. JSON output with card and action --------------------------------

func TestConversePrintReply_JSONWithCardAction(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	optJSON = true
	r := &conversation.Reply{
		Text:   "请审批",
		Card:   &chatops.Card{Kind: chatops.CardKindApproval, Title: "T", Summary: "S"},
		Action: &conversation.Action{Type: conversation.ActionApprove, Payload: map[string]string{"k": "v"}},
	}
	var buf bytes.Buffer
	require.NoError(t, printReplyJSON(&buf, "s1", conversation.StateReviewing, r))
	var env map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &env))
	data := env["data"].(map[string]any)
	reply := data["reply"].(map[string]any)
	assert.NotNil(t, reply["card"])
	assert.NotNil(t, reply["action"])
}

// --- 27. ensureSession + runConverseOnce integration ---------------------

func TestRunConverseOnce_PreservesSessionID(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	var buf bytes.Buffer
	require.NoError(t, runConverseOnce(engine, sess.ID, "alice", "hi", &buf))
	// The session id should appear in the output.
	assert.Contains(t, buf.String(), sess.ID)
}

// --- 28. EOF after blank line --------------------------------------------

func TestRunConverseInteractive_BlankThenEOF(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
}

// --- 29. /state after activity -------------------------------------------

func TestRunConverseInteractive_StateAfterMessage(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("hello\n/state\n/exit\n")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	s := out.String()
	// After one message the session is still idle (the engine ack-only).
	assert.Contains(t, s, "状态:       idle")
}

// --- 30. Cover the cobra RunE signature ----------------------------------

func TestRunConverse_AcceptsCobraArgs(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	cmd := &cobra.Command{}
	out, err := captureStdout(func() error {
		return runConverse(cmd, []string{"hi"})
	})
	require.NoError(t, err)
	assert.Contains(t, out, "💡 LEVEE:")
}

// --- 31. Edge cases for coverage -----------------------------------------

// TestRunConverseInteractive_SpaceThenEOF covers the branch where a line
// containing only whitespace is read together with EOF (no trailing newline).
func TestRunConverseInteractive_SpaceThenEOF(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("   ")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
}

// TestRunConverseInteractive_HelpThenEOFNoNewline covers the branch where a
// special command is followed immediately by EOF (no trailing newline).
func TestRunConverseInteractive_HelpThenEOFNoNewline(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("/help")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "可用命令")
}

// TestRunConverseInteractive_MessageThenEOFNoNewline covers the branch where
// a regular message is followed immediately by EOF (no trailing newline).
func TestRunConverseInteractive_MessageThenEOFNoNewline(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	in := strings.NewReader("hello")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, "", "alice", in, &out))
	assert.Contains(t, out.String(), "💡 LEVEE:")
}

// TestRunConverseInteractive_ErrorThenEOFNoNewline covers the branch where a
// HandleMessage error is followed immediately by EOF (no trailing newline).
func TestRunConverseInteractive_ErrorThenEOFNoNewline(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	// Create a session owned by carol, then interact as alice so
	// HandleMessage fails with "not the owner"; the input has no
	// trailing newline so EOF is returned together with the line.
	sess, err := engine.NewSession("carol")
	require.NoError(t, err)
	in := strings.NewReader("hi")
	var out bytes.Buffer
	require.NoError(t, runConverseInteractive(engine, sess.ID, "alice", in, &out))
	assert.Contains(t, out.String(), "❌")
}

// TestConverseHandleInteractiveCommand_NewError covers the /new failure
// branch (e.g. empty user id).
func TestConverseHandleInteractiveCommand_NewError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sessionID := sess.ID
	var out bytes.Buffer
	// Use an empty user id so NewSession fails.
	handled, err := handleInteractiveCommand(&out, engine, &sess, &sessionID, "  ", "/new")
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Contains(t, out.String(), "创建会话失败")
}

// TestRunConverse_DispatchHistorySuccess covers the --history success path
// by pre-creating a session in the global engine. Because runConverse
// creates its own engine, we instead verify the helper directly.
func TestRunConverse_DispatchHistorySuccess(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	defer engine.Close()
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sess.AddMessage(conversation.RoleUser, "ping")
	var buf bytes.Buffer
	require.NoError(t, printSessionHistory(&buf, sess))
	assert.Contains(t, buf.String(), "ping")
}

// TestRunConverse_DispatchInteractiveWithAlert covers the interactive path
// when --alert is set.
func TestRunConverse_DispatchInteractiveWithAlert(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseInteractive = true
	converseAlertID = "alert-99"
	origStdin := converseStdin
	converseStdin = strings.NewReader("/exit\n")
	defer func() { converseStdin = origStdin }()
	out, err := captureStdout(func() error {
		return runConverse(nil, nil)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "LEVEE 对话已启动")
}

// TestRunConverse_DispatchListJSON covers --list --json.
func TestRunConverse_DispatchListJSON(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	converseList = true
	out, err := captureStdout(func() error {
		optJSON = true
		return runConverse(nil, nil)
	})
	require.NoError(t, err)
	var env map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.NotNil(t, env["data"])
}

// TestRunConverse_DispatchHistoryWithPreExistingSession covers the --history
// success path by injecting a pre-populated engine.
func TestRunConverse_DispatchHistoryWithPreExistingSession(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	engine := newTestEngine(t)
	sess, err := engine.NewSession("alice")
	require.NoError(t, err)
	sess.AddMessage(conversation.RoleUser, "ping")
	origFactory := newConversationEngineForCLI
	newConversationEngineForCLI = func() (*conversation.ConversationEngine, error) {
		return engine, nil
	}
	defer func() { newConversationEngineForCLI = origFactory }()
	converseHistory = true
	converseSessionID = sess.ID
	out, err := captureStdout(func() error {
		return runConverse(nil, nil)
	})
	require.NoError(t, err)
	assert.Contains(t, out, "ping")
}

// TestRunConverse_EngineInitError covers the engine creation error path.
func TestRunConverse_EngineInitError(t *testing.T) {
	defer resetRootFlags()
	defer resetConverseFlags()
	origFactory := newConversationEngineForCLI
	newConversationEngineForCLI = func() (*conversation.ConversationEngine, error) {
		return nil, errors.New("boom")
	}
	defer func() { newConversationEngineForCLI = origFactory }()
	err := runConverse(nil, []string{"hi"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "init engine")
}

// Suppress unused-import warnings for io in case the test file evolves.
var _ = io.EOF
var _ = context.Background