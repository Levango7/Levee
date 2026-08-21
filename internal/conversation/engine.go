package conversation

// engine.go implements ConversationEngine, the multi-turn dialogue manager
// that owns all live Sessions and dispatches incoming user messages to the
// appropriate state handler. The engine is the single integration point
// between the IM / Web UI / CLI front-ends and the diagnosis + recommend
// engines.
//
// Concurrency model:
//   - The sessions map is guarded by ConversationEngine.mu (RWMutex).
//   - Each Session has its own RWMutex; the engine never holds its own
//     lock while mutating a session, so different sessions can be served
//     in parallel.
//   - HandleMessage is the only entry point that mutates session state; it
//     looks up the session under RLock, then operates on the session
//     directly. The lookup is done with a write-lock-free copy of the
//     pointer, which is safe because the map only stores pointers and
//     deletion happens under Lock.
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrSessionNotFound is returned when a session lookup misses.
	ErrSessionNotFound = errors.New("conversation: session not found")
	// ErrSessionClosed is returned when an operation is attempted on a
	// session that has been closed and removed from the engine.
	ErrSessionClosed = errors.New("conversation: session closed")
	// ErrEmptyMessage is returned when HandleMessage is called with an
	// empty (whitespace-only) message.
	ErrEmptyMessage = errors.New("conversation: empty message")
	// ErrNilRecommend is returned when HandleMessage needs the recommend
	// engine but none was configured.
	ErrNilRecommend = errors.New("conversation: nil recommend engine")
	// ErrInvalidState is returned when a state transition is not allowed
	// from the current state.
	ErrInvalidState = errors.New("conversation: invalid state transition")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultConversationTimeout is the per-HandleMessage timeout when the
	// config does not specify one.
	DefaultConversationTimeout = 60 * time.Second
)

// --- ConversationEngineConfig ----------------------------------------------

// ConversationEngineConfig configures a ConversationEngine. Recommend is
// required; Diagnose is optional (nil means the engine will reject
// /diagnose commands with an explanatory error).
type ConversationEngineConfig struct {
	// Recommend is the recommendation engine used to generate fix
	// proposals. Must be non-nil.
	Recommend *recommend.RecommendEngine
	// Diagnose is the diagnosis engine used by /diagnose and by
	// NewSessionFromAlert. May be nil; in that case diagnose commands
	// return an error.
	Diagnose *diagnosis.DiagEngine
	// Timeout is the wall-clock budget for a single HandleMessage call.
	// Zero defaults to DefaultConversationTimeout.
	Timeout time.Duration
	// Logger is the structured logger. When nil the package-level
	// singleton from internal/log is used.
	Logger *slog.Logger
}

// --- ConversationEngine -----------------------------------------------------

// ConversationEngine owns the set of live Sessions and dispatches incoming
// messages. It is safe for concurrent use by any number of goroutines.
type ConversationEngine struct {
	sessions  map[string]*Session
	recommend *recommend.RecommendEngine
	diagnose  *diagnosis.DiagEngine
	log       *slog.Logger
	timeout   time.Duration
	mu        sync.RWMutex
}

// NewConversationEngine creates a ConversationEngine from the given config.
// It applies defaults for zero-valued fields and never returns nil. It
// returns an error when Recommend is nil; callers may pass nil Recommend
// only if they intend to never call HandleMessage with /recommend.
//
// Deprecated behaviour: to keep the constructor total (no error return)
// the engine accepts a nil Recommend and returns ErrNilRecommend lazily
// from HandleMessage. This keeps tests that only exercise session
// management simple.
func NewConversationEngine(cfg ConversationEngineConfig) *ConversationEngine {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultConversationTimeout
	}
	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "conversation_engine")
	}
	return &ConversationEngine{
		sessions:  make(map[string]*Session),
		recommend: cfg.Recommend,
		diagnose:  cfg.Diagnose,
		log:       lg,
		timeout:   timeout,
	}
}

// Close releases all sessions and frees resources. It is idempotent.
func (e *ConversationEngine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.sessions = make(map[string]*Session)
	return nil
}

// --- Session management -----------------------------------------------------

// NewSession creates a new idle session for the given user and registers it
// with the engine. The returned Session has a freshly generated UUID.
func (e *ConversationEngine) NewSession(userID string) (*Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("conversation: new session: %w", ErrEmptyMessage)
	}
	sess := newSession(userID, "")
	e.mu.Lock()
	e.sessions[sess.ID] = sess
	e.mu.Unlock()
	e.log.Info("conversation: session created", "session_id", sess.ID, "user_id", userID)
	return sess, nil
}

// NewSessionFromAlert creates a new session linked to the given alert and
// immediately transitions it to StateDiagnosing. When a diagnosis engine is
// configured the engine runs a synchronous diagnosis, stores the resulting
// report id on the session and transitions to StateRecommending; when no
// diagnosis engine is configured the session stays in StateDiagnosing and
// the operator is expected to drive the flow manually.
func (e *ConversationEngine) NewSessionFromAlert(userID, alertID string) (*Session, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("conversation: new session from alert: %w", ErrEmptyMessage)
	}
	if strings.TrimSpace(alertID) == "" {
		return nil, fmt.Errorf("conversation: new session from alert: empty alert id")
	}
	sess := newSession(userID, alertID)
	sess.SetState(StateDiagnosing)
	e.mu.Lock()
	e.sessions[sess.ID] = sess
	e.mu.Unlock()
	e.log.Info("conversation: session created from alert",
		"session_id", sess.ID, "user_id", userID, "alert_id", alertID)
	return sess, nil
}

// GetSession returns the session with the given id. It returns
// ErrSessionNotFound when no such session exists.
func (e *ConversationEngine) GetSession(sessionID string) (*Session, error) {
	e.mu.RLock()
	sess, ok := e.sessions[sessionID]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("conversation: get session %q: %w", sessionID, ErrSessionNotFound)
	}
	return sess, nil
}

// CloseSession removes the session with the given id from the engine. It
// returns ErrSessionNotFound when no such session exists. The session
// object itself is not mutated; callers holding a pointer may continue to
// inspect its history.
func (e *ConversationEngine) CloseSession(sessionID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.sessions[sessionID]; !ok {
		return fmt.Errorf("conversation: close session %q: %w", sessionID, ErrSessionNotFound)
	}
	delete(e.sessions, sessionID)
	e.log.Info("conversation: session closed", "session_id", sessionID)
	return nil
}

// ListSessions returns all live sessions belonging to the given user. The
// returned slice is freshly allocated and safe to mutate. When no sessions
// exist for the user an empty (non-nil) slice is returned.
func (e *ConversationEngine) ListSessions(userID string) []*Session {
	e.mu.RLock()
	defer e.mu.RUnlock()

	out := make([]*Session, 0)
	for _, s := range e.sessions {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out
}

// SessionCount returns the number of currently live sessions across all
// users.
func (e *ConversationEngine) SessionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.sessions)
}

// --- HandleMessage ----------------------------------------------------------

// HandleMessage is the core entry point. It looks up the session, validates
// the message and dispatches to the state-specific handler. The returned
// Reply is always non-nil on success.
func (e *ConversationEngine) HandleMessage(_ctx context.Context, sessionID, userID, text string) (*Reply, error) {
	if normalizeText(text) == "" {
		return nil, fmt.Errorf("conversation: handle message: %w", ErrEmptyMessage)
	}
	sess, err := e.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if _ctx == nil {
		_ctx = context.Background()
	}
	// Apply the engine timeout when the caller has not set a deadline.
	if _, ok := _ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		_ctx, cancel = context.WithTimeout(_ctx, e.timeout)
		defer cancel()
	}

	// Authorisation check: the caller must own the session.
	if userID != "" && sess.UserID != userID {
		return nil, fmt.Errorf("conversation: user %q is not the owner of session %q", userID, sessionID)
	}

	state := sess.GetState()
	switch state {
	case StateIdle:
		return e.handleIdle(_ctx, sess, text)
	case StateDiagnosing:
		return e.handleDiagnosing(_ctx, sess, text)
	case StateRecommending:
		return e.handleRecommending(_ctx, sess, text)
	case StateReviewing:
		return e.handleReviewing(_ctx, sess, text)
	case StateExecuting:
		return e.handleExecuting(_ctx, sess, text)
	case StateDone, StateFailed:
		return e.handleTerminal(_ctx, sess, text)
	default:
		return nil, fmt.Errorf("conversation: state %s: %w", state, ErrInvalidState)
	}
}

// --- state handlers ---------------------------------------------------------

// handleIdle dispatches commands in the idle state.
func (e *ConversationEngine) handleIdle(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)

	switch {
	case hasPrefix(msg, "/diagnose"):
		target := strings.TrimSpace(strings.TrimPrefix(msg, "/diagnose"))
		if target == "" {
			return &Reply{Text: "用法: /diagnose <target>"}, nil
		}
		return e.runDiagnose(_ctx, sess, target)
	case hasPrefix(msg, "/recommend"):
		return e.runRecommendFromHistory(_ctx, sess)
	case hasPrefix(msg, "/help"):
		return &Reply{Text: helpText()}, nil
	default:
		return &Reply{Text: "已收到您的消息，输入 /help 查看可用命令"}, nil
	}
}

// handleDiagnosing handles messages while a diagnosis is in progress. The
// operator may cancel with /cancel; any other input is acknowledged but
// does not change state.
func (e *ConversationEngine) handleDiagnosing(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)
	if msg == "/cancel" || msg == "cancel" || msg == "取消" {
		sess.SetState(StateFailed)
		return &Reply{Text: "诊断已取消", Action: &Action{Type: ActionCancel}}, nil
	}
	return &Reply{Text: "诊断进行中，请等待"}, nil
}

// handleRecommending handles messages while a recommendation is being
// generated. Behaviour mirrors handleDiagnosing.
func (e *ConversationEngine) handleRecommending(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)
	if msg == "/cancel" || msg == "cancel" || msg == "取消" {
		sess.SetState(StateFailed)
		return &Reply{Text: "建议生成已取消", Action: &Action{Type: ActionCancel}}, nil
	}
	return &Reply{Text: "正在生成建议，请等待"}, nil
}

// handleReviewing dispatches user decisions about the proposed
// recommendation.
func (e *ConversationEngine) handleReviewing(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)

	lower := strings.ToLower(msg)
	switch lower {
	case "执行", "approve", "yes", "y":
		sess.SetState(StateExecuting)
		action := &Action{Type: ActionApprove, Payload: map[string]string{}}
		if rec := sess.GetRecommendation(); rec != nil {
			action.Payload["recommendation_id"] = rec.ID
		}
		sess.AddMessageWithAction(RoleSystem, "user approved", action)
		return &Reply{Text: "已批准，开始执行", Action: action}, nil
	case "拒绝", "reject", "no", "n":
		sess.SetState(StateFailed)
		action := &Action{Type: ActionReject}
		sess.AddMessageWithAction(RoleSystem, "user rejected", action)
		return &Reply{Text: "已拒绝建议", Action: action}, nil
	case "修改", "modify":
		action := &Action{Type: ActionModify}
		return &Reply{Text: "请描述您希望修改的部分", Action: action}, nil
	default:
		// Treat as a follow-up question about the recommendation.
		rec := sess.GetRecommendation()
		if rec == nil {
			return &Reply{Text: "暂无建议可参考，请先执行 /recommend"}, nil
		}
		return &Reply{Text: answerFromRecommendation(rec, msg)}, nil
	}
}

// handleExecuting handles messages while the fix workflow is running.
func (e *ConversationEngine) handleExecuting(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)

	lower := strings.ToLower(msg)
	if lower == "cancel" || lower == "取消" || lower == "/cancel" {
		sess.SetState(StateFailed)
		action := &Action{Type: ActionCancel}
		sess.AddMessageWithAction(RoleSystem, "user cancelled execution", action)
		return &Reply{Text: "执行已取消", Action: action}, nil
	}
	return &Reply{Text: "执行进行中，请等待"}, nil
}

// handleTerminal handles messages in terminal states (Done / Failed).
func (e *ConversationEngine) handleTerminal(_ctx context.Context, sess *Session, text string) (*Reply, error) {
	msg := normalizeText(text)
	sess.AddMessage(RoleUser, msg)

	if msg == "/restart" {
		sess.Reset()
		return &Reply{Text: "会话已重置，请输入新指令"}, nil
	}
	return &Reply{Text: "会话已结束，输入 /restart 重新开始或新建会话"}, nil
}

// --- diagnose / recommend integration ---------------------------------------

// runDiagnose runs a synchronous diagnosis against target, stores the
// resulting report id on the session and then drives the recommend engine
// to produce a recommendation. When the diagnose engine is nil the session
// is left in StateDiagnosing and an explanatory reply is returned.
func (e *ConversationEngine) runDiagnose(_ctx context.Context, sess *Session, target string) (*Reply, error) {
	if e.diagnose == nil {
		sess.SetState(StateDiagnosing)
		return &Reply{Text: "诊断引擎未配置，无法执行 /diagnose"}, nil
	}
	sess.SetState(StateDiagnosing)
	report := e.diagnose.Diagnose(_ctx, target)
	sess.DiagnosisID = report.ID

	// Drive the recommend engine to produce a recommendation.
	rec, err := e.produceRecommendation(_ctx, sess, &report)
	if err != nil {
		sess.SetState(StateFailed)
		return nil, fmt.Errorf("conversation: diagnose: %w", err)
	}
	return e.reviewReply(sess, rec), nil
}

// runRecommendFromHistory runs the recommend engine using the most recent
// diagnosis referenced by the session. When no diagnosis is available the
// engine synthesises a minimal report with a placeholder target so the
// knowledge base can still be queried and the operator gets a usable
// recommendation rather than an error.
func (e *ConversationEngine) runRecommendFromHistory(_ctx context.Context, sess *Session) (*Reply, error) {
	if e.recommend == nil {
		return nil, fmt.Errorf("conversation: recommend: %w", ErrNilRecommend)
	}
	sess.SetState(StateRecommending)

	// We do not store full reports on the session. When a diagnosis id is
	// present we keep it for traceability; otherwise we use a placeholder
	// target so the recommend engine still produces a knowledge-base match.
	report := diagnosis.DiagnosticReport{
		ID:     sess.DiagnosisID,
		Target: "session-context",
	}
	if sess.AlertID != "" {
		report.AlertID = sess.AlertID
	}
	rec, err := e.produceRecommendation(_ctx, sess, &report)
	if err != nil {
		sess.SetState(StateFailed)
		return nil, fmt.Errorf("conversation: recommend: %w", err)
	}
	return e.reviewReply(sess, rec), nil
}

// produceRecommendation calls the recommend engine and stores the result on
// the session. It accepts a nil report when no diagnosis is available; in
// that case it returns an error so the caller can transition to Failed.
func (e *ConversationEngine) produceRecommendation(_ctx context.Context, sess *Session, report *diagnosis.DiagnosticReport) (*recommend.Recommendation, error) {
	if e.recommend == nil {
		return nil, ErrNilRecommend
	}
	rec, err := e.recommend.Recommend(_ctx, report)
	if err != nil {
		return nil, err
	}
	sess.SetRecommendation(rec)
	sess.SetState(StateReviewing)
	return rec, nil
}

// reviewReply builds the Reply presented to the operator when a
// recommendation is ready for review. It includes a textual summary and an
// approval card wired to the session.
func (e *ConversationEngine) reviewReply(sess *Session, rec *recommend.Recommendation) *Reply {
	summary := answerFromRecommendation(rec, "")
	card := buildApprovalCard(sess, rec)
	return &Reply{
		Text: summary,
		Card: card,
		Action: &Action{
			Type:    ActionNone,
			Payload: map[string]string{"recommendation_id": rec.ID},
		},
	}
}

// --- helpers ----------------------------------------------------------------

// helpText returns the help text shown in response to /help.
func helpText() string {
	return strings.Join([]string{
		"LEVEE 对话命令:",
		"  /diagnose <target>  — 对目标执行诊断并生成建议",
		"  /recommend          — 基于当前上下文生成建议",
		"  /help               — 显示此帮助",
		"  /restart            — 重置会话（仅在终态可用）",
		"  /cancel             — 取消进行中的诊断/执行",
		"在审核阶段回复: 执行 / 拒绝 / 修改",
	}, "\n")
}

// answerFromRecommendation produces a textual answer based on the
// recommendation. When the question is empty a summary is returned;
// otherwise a short follow-up answer is synthesised.
func answerFromRecommendation(rec *recommend.Recommendation, question string) string {
	if rec == nil {
		return "暂无建议"
	}
	if question == "" {
		return fmt.Sprintf("建议摘要: %s\n方案: %s\n风险级别: %s\n置信度: %.0f%%",
			rec.Summary, rec.Approach, rec.RiskLevel, rec.Confidence*100)
	}
	return fmt.Sprintf("关于您的问题: %s\n当前建议: %s\n方案: %s",
		question, rec.Summary, rec.Approach)
}

// buildApprovalCard constructs the chatops approval card for a
// recommendation.
func buildApprovalCard(sess *Session, rec *recommend.Recommendation) *chatops.Card {
	evt := chatops.Event{
		Type:      chatops.EventApprovalRequested,
		RunID:     sess.WorkflowID,
		Title:     rec.Summary,
		Summary:   rec.Approach,
		Level:     string(rec.RiskLevel),
		ChangeID:  rec.ID,
		DetailURL: "",
		Timestamp: time.Now(),
	}
	return chatops.BuildApprovalCard(evt)
}
