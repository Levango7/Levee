package main

// cmd_converse.go implements the `levee converse` sub-command (Phase B7).
// It exposes the multi-turn ConversationEngine directly in the terminal so
// operators can drive a remediation dialogue without leaving the CLI. Two
// modes are supported:
//
//   - Single-shot:  levee converse "web-01 CPU 使用率过高"
//     Send one message, print the reply, exit.
//
//   - Interactive:  levee converse --interactive
//     Enter a REPL loop. Special commands (/exit, /help, /state, /history,
//     /sessions, /new) are interpreted locally; everything else is forwarded
//     to the engine. EOF (Ctrl-D) also exits the loop.
//
// The command always wires a default RecommendEngine so /recommend works out
// of the box. The Diagnose engine is left nil when not configured; in that
// case /diagnose returns an explanatory error from the engine.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/conversation"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrConverseNoMessage is returned when the single-shot mode is invoked
	// without a positional message argument.
	ErrConverseNoMessage = errors.New("converse: no message provided")
	// ErrConverseSessionLost is returned when an operation needs a Session
	// pointer but the one we cached has gone missing.
	ErrConverseSessionLost = errors.New("converse: session lost")
)

// --- Command flags ----------------------------------------------------------

var (
	converseInteractive bool   // --interactive/-i
	converseSessionID   string // --session
	converseUserID      string // --user
	converseAlertID     string // --alert
	converseHistory     bool   // --history
	converseList        bool   // --list
)

// DefaultConverseUserID is the default user id when --user is not provided.
const DefaultConverseUserID = "cli-user"

// converseStdin is the input stream used by the interactive mode. It defaults
// to os.Stdin and is exposed as a package-level variable so tests can swap
// it for a deterministic reader without touching the process-global fd.
var converseStdin io.Reader = os.Stdin

func init() {
	RegisterCommand(newConverseCmd())
}

// newConverseCmd builds the `levee converse [message]` sub-command.
func newConverseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "converse [message]",
		Short: "与 LEVEE 对话引擎交互",
		Long: "与 LEVEE AI 对话引擎交互，支持单次消息和交互式对话。\n\n" +
			"单次模式：levee converse \"web-01 CPU 使用率过高\"\n" +
			"交互模式：levee converse --interactive\n" +
			"指定会话：levee converse --session <session-id> \"继续诊断\"",
		Args: cobra.MaximumNArgs(1),
		RunE: runConverse,
	}
	cmd.Flags().BoolVarP(&converseInteractive, "interactive", "i", false, "交互模式，进入 REPL 循环")
	cmd.Flags().StringVar(&converseSessionID, "session", "", "指定已有会话 ID")
	cmd.Flags().StringVar(&converseUserID, "user", DefaultConverseUserID, "用户 ID")
	cmd.Flags().StringVar(&converseAlertID, "alert", "", "从告警 ID 创建会话")
	cmd.Flags().BoolVar(&converseHistory, "history", false, "显示会话历史并退出")
	cmd.Flags().BoolVar(&converseList, "list", false, "列出所有活跃会话并退出")
	return cmd
}

// --- Command entry point ----------------------------------------------------

// runConverse is the cobra RunE entry point. It dispatches to the appropriate
// sub-flow based on the flags:
//   - --list                  -> list sessions and exit
//   - --history --session=X   -> print session history and exit
//   - --interactive           -> REPL loop
//   - otherwise               -> single-shot message
func runConverse(cmd *cobra.Command, args []string) error {
	userID := converseUserID
	if strings.TrimSpace(userID) == "" {
		userID = DefaultConverseUserID
	}

	engine, err := newConversationEngineForCLI()
	if err != nil {
		return fmt.Errorf("converse: init engine: %w", err)
	}
	defer func() { _ = engine.Close() }()

	// --list: list sessions and exit.
	if converseList {
		return runConverseList(engine, userID, os.Stdout)
	}

	// --history requires --session.
	if converseHistory {
		if converseSessionID == "" {
			return fmt.Errorf("converse: --history requires --session")
		}
		sess, err := engine.GetSession(converseSessionID)
		if err != nil {
			return fmt.Errorf("converse: %w", err)
		}
		return printSessionHistory(os.Stdout, sess)
	}

	if converseInteractive {
		return runConverseInteractive(engine, converseSessionID, userID, converseStdin, os.Stdout)
	}

	// Single-shot mode requires a positional message.
	if len(args) == 0 {
		return fmt.Errorf("converse: %w", ErrConverseNoMessage)
	}
	return runConverseOnce(engine, converseSessionID, userID, args[0], os.Stdout)
}

// --- Engine factory ---------------------------------------------------------

// newConversationEngineForCLI builds a ConversationEngine using sensible CLI
// defaults. The RecommendEngine is always wired with the built-in knowledge
// base so /recommend works out of the box; the Diagnose engine is left nil
// (the engine returns an explanatory error for /diagnose in that case).
//
// It is exposed as a package-level variable so tests can substitute a
// pre-populated engine (e.g. one that already contains a session) without
// touching the production call site.
var newConversationEngineForCLI = defaultNewConversationEngineForCLI

func defaultNewConversationEngineForCLI() (*conversation.ConversationEngine, error) {
	recEngine := recommend.NewRecommendEngine(recommend.RecommendEngineConfig{
		Timeout: 30 * time.Second,
	})
	return conversation.NewConversationEngine(conversation.ConversationEngineConfig{
		Recommend: recEngine,
		Timeout:   60 * time.Second,
	}), nil
}

// --- Single-shot mode -------------------------------------------------------

// runConverseOnce handles the single-message mode. When sessionID is empty a
// fresh session is created (optionally from an alert when converseAlertID is
// set); otherwise the existing session is reused.
func runConverseOnce(engine *conversation.ConversationEngine, sessionID, userID, message string, w io.Writer) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("converse: %w", ErrConverseNoMessage)
	}

	sess, err := ensureSession(engine, sessionID, userID)
	if err != nil {
		return err
	}
	sessionID = sess.ID

	ctx := context.Background()
	reply, err := engine.HandleMessage(ctx, sessionID, userID, message)
	if err != nil {
		return fmt.Errorf("converse: handle message: %w", err)
	}

	if optJSON {
		return printReplyJSON(w, sessionID, sess.GetState(), reply)
	}
	return printReply(w, reply, sessionID, sess.GetState())
}

// ensureSession returns an existing session when sessionID is non-empty, or
// creates a new one (optionally from an alert) otherwise.
func ensureSession(engine *conversation.ConversationEngine, sessionID, userID string) (*conversation.Session, error) {
	if sessionID == "" {
		if converseAlertID != "" {
			sess, err := engine.NewSessionFromAlert(userID, converseAlertID)
			if err != nil {
				return nil, fmt.Errorf("converse: new session from alert: %w", err)
			}
			return sess, nil
		}
		sess, err := engine.NewSession(userID)
		if err != nil {
			return nil, fmt.Errorf("converse: new session: %w", err)
		}
		return sess, nil
	}
	sess, err := engine.GetSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("converse: %w", err)
	}
	return sess, nil
}

// --- Interactive mode -------------------------------------------------------

// runConverseInteractive runs the REPL loop. Special commands are interpreted
// locally; everything else is forwarded to the engine. The loop exits on
// /exit, /quit or EOF.
func runConverseInteractive(engine *conversation.ConversationEngine, sessionID, userID string, in io.Reader, out io.Writer) error {
	sess, err := ensureSession(engine, sessionID, userID)
	if err != nil {
		return err
	}
	sessionID = sess.ID

	fmt.Fprintf(out, "LEVEE 对话已启动（会话 %s）\n输入 /help 查看可用命令，/exit 退出\n", sessionID)

	reader := bufio.NewReader(in)
	ctx := context.Background()
	for {
		fmt.Fprint(out, "levee> ")
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			if readErr == io.EOF {
				if line == "" {
					fmt.Fprintln(out)
					return nil
				}
				// Process the trailing line then exit.
			} else {
				return fmt.Errorf("converse: read input: %w", readErr)
			}
		}
		line = strings.TrimSpace(line)
		if line == "" {
			if readErr == io.EOF {
				return nil
			}
			continue
		}

		// Local special commands.
		handled, err := handleInteractiveCommand(out, engine, &sess, &sessionID, userID, line)
		if err != nil {
			if errors.Is(err, errConverseExit) {
				return nil
			}
			return err
		}
		if handled {
			if readErr == io.EOF {
				return nil
			}
			continue
		}

		// Forward to the engine.
		reply, hErr := engine.HandleMessage(ctx, sessionID, userID, line)
		if hErr != nil {
			fmt.Fprintf(out, "❌ 错误: %v\n", hErr)
			if readErr == io.EOF {
				return nil
			}
			continue
		}
		if pErr := printReply(out, reply, sessionID, sess.GetState()); pErr != nil {
			return pErr
		}
		if readErr == io.EOF {
			return nil
		}
	}
}

// handleInteractiveCommand interprets one local special command. It returns
// handled=true when the line was a recognised special command (whether or not
// it succeeded); the caller should not forward such lines to the engine.
func handleInteractiveCommand(out io.Writer, engine *conversation.ConversationEngine, sess **conversation.Session, sessionID *string, userID, line string) (bool, error) {
	switch line {
	case "/exit", "/quit":
		fmt.Fprintln(out, "再见！")
		return true, errConverseExit
	case "/help":
		return true, printHelp(out)
	case "/state":
		return true, printSessionInfo(out, *sess)
	case "/history":
		return true, printSessionHistory(out, *sess)
	case "/sessions":
		return true, printSessionList(out, engine, userID)
	case "/new":
		newSess, err := engine.NewSession(userID)
		if err != nil {
			fmt.Fprintf(out, "❌ 创建会话失败: %v\n", err)
			return true, nil
		}
		*sess = newSess
		*sessionID = newSess.ID
		fmt.Fprintf(out, "✨ 新会话已创建: %s\n", *sessionID)
		return true, nil
	}
	return false, nil
}

// errConverseExit is the internal sentinel that lets runConverseInteractive
// break out of the loop on /exit without duplicating the return path.
var errConverseExit = errors.New("converse: exit requested")

// --- Output helpers ---------------------------------------------------------

// printReply writes a reply in human-readable form to w.
func printReply(w io.Writer, reply *conversation.Reply, sessionID string, state conversation.SessionState) error {
	if reply == nil {
		fmt.Fprintln(w, "💡 LEVEE: <空回复>")
		fmt.Fprintf(w, "   [会话: %s | 状态: %s]\n", sessionID, state.String())
		return nil
	}
	fmt.Fprintf(w, "💡 LEVEE: %s\n", reply.Text)
	if reply.Card != nil {
		summary := formatCard(reply.Card)
		fmt.Fprintf(w, "   [卡片: %s]\n", summary)
	}
	if reply.Action != nil && reply.Action.Type != conversation.ActionNone {
		fmt.Fprintf(w, "   [动作: %s]\n", reply.Action.Type)
	}
	fmt.Fprintf(w, "   [会话: %s | 状态: %s]\n", sessionID, state.String())
	return nil
}

// printReplyJSON writes a reply as a JSON envelope to w.
func printReplyJSON(w io.Writer, sessionID string, state conversation.SessionState, reply *conversation.Reply) error {
	return PrintJSON(w, map[string]any{
		"data": map[string]any{
			"session_id": sessionID,
			"state":      state.String(),
			"reply":      reply,
		},
		"meta":  nil,
		"error": nil,
	})
}

// printSessionInfo writes session metadata to w.
func printSessionInfo(w io.Writer, sess *conversation.Session) error {
	if sess == nil {
		return fmt.Errorf("converse: %w", ErrConverseSessionLost)
	}
	fmt.Fprintf(w, "会话 ID:    %s\n", sess.ID)
	fmt.Fprintf(w, "用户 ID:    %s\n", sess.UserID)
	fmt.Fprintf(w, "状态:       %s\n", sess.GetState().String())
	if sess.AlertID != "" {
		fmt.Fprintf(w, "告警 ID:    %s\n", sess.AlertID)
	}
	if sess.DiagnosisID != "" {
		fmt.Fprintf(w, "诊断 ID:    %s\n", sess.DiagnosisID)
	}
	if sess.WorkflowID != "" {
		fmt.Fprintf(w, "工作流 ID:  %s\n", sess.WorkflowID)
	}
	fmt.Fprintf(w, "消息数:     %d\n", len(sess.History()))
	return nil
}

// printSessionHistory writes the message history of a session to w.
func printSessionHistory(w io.Writer, sess *conversation.Session) error {
	if sess == nil {
		return fmt.Errorf("converse: %w", ErrConverseSessionLost)
	}
	history := sess.History()
	if len(history) == 0 {
		fmt.Fprintln(w, "（无历史消息）")
		return nil
	}
	fmt.Fprintf(w, "会话 %s 历史（%d 条消息）:\n", sess.ID, len(history))
	for _, msg := range history {
		fmt.Fprintf(w, "  [%s] %s: %s\n", msg.Timestamp.Format("15:04:05"), msg.Role, msg.Content)
	}
	return nil
}

// printSessionList lists all live sessions for the user.
func printSessionList(w io.Writer, engine *conversation.ConversationEngine, userID string) error {
	sessions := engine.ListSessions(userID)
	if len(sessions) == 0 {
		fmt.Fprintln(w, "（无活跃会话）")
		return nil
	}
	fmt.Fprintf(w, "用户 %s 的活跃会话（%d）:\n", userID, len(sessions))
	for _, s := range sessions {
		fmt.Fprintf(w, "  %s  状态=%s  消息=%d\n", s.ID, s.GetState().String(), len(s.History()))
	}
	return nil
}

// runConverseList handles the --list flag. It supports both human and JSON
// output.
func runConverseList(engine *conversation.ConversationEngine, userID string, w io.Writer) error {
	sessions := engine.ListSessions(userID)
	if optJSON {
		data := make([]map[string]any, 0, len(sessions))
		for _, s := range sessions {
			data = append(data, map[string]any{
				"id":    s.ID,
				"state": s.GetState().String(),
				"msgs":  len(s.History()),
				"alert": s.AlertID,
			})
		}
		return PrintJSON(w, map[string]any{
			"data":  data,
			"meta":  map[string]any{"user_id": userID, "count": len(data)},
			"error": nil,
		})
	}
	return printSessionList(w, engine, userID)
}

// printHelp writes the interactive-mode help text to w.
func printHelp(w io.Writer) error {
	const help = `可用命令：
  /exit, /quit  - 退出对话
  /help         - 显示帮助
  /state        - 显示当前会话状态
  /history      - 显示对话历史
  /sessions     - 列出所有活跃会话
  /new          - 创建新会话
  其他文本      - 发送给对话引擎
`
	fmt.Fprint(w, help)
	return nil
}

// formatCard formats a Card as a terminal-friendly one-line summary. It
// returns an empty string when the card is nil.
func formatCard(card *chatops.Card) string {
	if card == nil {
		return ""
	}
	if card.Title == "" && card.Summary == "" {
		return string(card.Kind)
	}
	if card.Summary == "" {
		return card.Title
	}
	if card.Title == "" {
		return card.Summary
	}
	return card.Title + " - " + card.Summary
}
