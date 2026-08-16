package conversation

// Package conversation implements the multi-turn conversation engine that
// manages interactive remediation dialogues between operators and LEVEE.
//
// The engine owns a set of Sessions, each of which carries a state machine
// (Idle -> Diagnosing -> Recommending -> Reviewing -> Executing -> Done/Failed)
// plus the message history and the current Recommendation under review. It
// dispatches incoming user messages to the appropriate handler based on the
// session's current state, integrates with the diagnosis and recommend engines
// and produces Reply values that the IM / Web UI / CLI front-ends render.
//
// All public types are safe for concurrent use. The engine never panics;
// errors are propagated through error returns.
import (
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nexus/levee/internal/chatops"
	"github.com/nexus/levee/internal/recommend"
)

// --- SessionState -----------------------------------------------------------

// SessionState is the current state of a conversation session's state machine.
// The legal transitions are documented on ConversationEngine.HandleMessage.
type SessionState int

const (
	// StateIdle means the session is waiting for user input.
	StateIdle SessionState = iota
	// StateDiagnosing means a diagnosis run is in progress.
	StateDiagnosing
	// StateRecommending means a recommendation is being generated.
	StateRecommending
	// StateReviewing means the engine is waiting for the user to approve,
	// reject or modify the proposed recommendation.
	StateReviewing
	// StateExecuting means the approved fix workflow is running.
	StateExecuting
	// StateDone means the session has completed successfully.
	StateDone
	// StateFailed means the session has failed (rejected, cancelled or
	// errored).
	StateFailed
)

// String returns the human-readable name of the state.
func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateDiagnosing:
		return "diagnosing"
	case StateRecommending:
		return "recommending"
	case StateReviewing:
		return "reviewing"
	case StateExecuting:
		return "executing"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// --- MessageRole ------------------------------------------------------------

// MessageRole identifies who produced a Message.
type MessageRole string

const (
	// RoleUser marks a message from the human operator.
	RoleUser MessageRole = "user"
	// RoleAssistant marks a message from LEVEE.
	RoleAssistant MessageRole = "assistant"
	// RoleSystem marks an internal system message (e.g. state transitions).
	RoleSystem MessageRole = "system"
)

// --- ActionType -------------------------------------------------------------

// ActionType identifies the kind of action attached to a Reply or Message.
// Front-ends use it to render the appropriate interactive surface (e.g.
// approve / reject buttons).
type ActionType string

const (
	// ActionApprove means the user approved the recommendation.
	ActionApprove ActionType = "approve"
	// ActionReject means the user rejected the recommendation.
	ActionReject ActionType = "reject"
	// ActionExecute means the user requested execution of the workflow.
	ActionExecute ActionType = "execute"
	// ActionModify means the user requested modification of the recommendation.
	ActionModify ActionType = "modify"
	// ActionCancel means the user cancelled an in-progress execution.
	ActionCancel ActionType = "cancel"
	// ActionNone means no action is attached; the reply is purely textual.
	ActionNone ActionType = "none"
)

// --- Action -----------------------------------------------------------------

// Action is an optional interactive payload attached to a Reply or Message.
// It carries the action type plus a free-form key/value payload that the
// front-end can use to render buttons or forms.
type Action struct {
	Type    ActionType        `json:"type"`
	Payload map[string]string `json:"payload,omitempty"`
}

// --- Message ----------------------------------------------------------------

// Message is a single entry in a session's conversation history.
type Message struct {
	ID        string      `json:"id"`
	Role      MessageRole `json:"role"`
	Content   string      `json:"content"`
	Timestamp time.Time   `json:"timestamp"`
	Action    *Action     `json:"action,omitempty"`
}

// --- Reply ------------------------------------------------------------------

// Reply is the value returned by ConversationEngine.HandleMessage. It carries
// the textual reply, an optional interactive Card (e.g. approval buttons) and
// an optional Action that the front-end can dispatch on.
type Reply struct {
	Text   string         `json:"text"`
	Card   *chatops.Card  `json:"card,omitempty"`
	Action *Action        `json:"action,omitempty"`
}

// --- Session ----------------------------------------------------------------

// Session is a single multi-turn conversation. It owns the message history,
// the current state-machine state and the optional Recommendation under
// review. All mutators are guarded by an internal RWMutex so a Session is
// safe for concurrent use.
type Session struct {
	ID             string                     `json:"id"`
	UserID         string                     `json:"user_id"`
	AlertID        string                     `json:"alert_id,omitempty"`
	DiagnosisID    string                     `json:"diagnosis_id,omitempty"`
	Recommendation *recommend.Recommendation  `json:"recommendation,omitempty"`
	Messages       []Message                  `json:"messages"`
	State          SessionState               `json:"state"`
	WorkflowID     string                     `json:"workflow_id,omitempty"`
	CreatedAt      time.Time                  `json:"created_at"`
	UpdatedAt      time.Time                  `json:"updated_at"`

	mu sync.RWMutex
}

// newSession constructs a fresh Session with a UUID and the given user/alert
// ids. It is intended for use by ConversationEngine; callers should use
// NewSession / NewSessionFromAlert instead.
func newSession(userID, alertID string) *Session {
	now := time.Now()
	return &Session{
		ID:        uuid.NewString(),
		UserID:    userID,
		AlertID:   alertID,
		State:     StateIdle,
		Messages:  make([]Message, 0, 8),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// AddMessage appends a message with the given role and content to the
// session history and returns the constructed Message. The message ID is a
// freshly generated UUID.
func (s *Session) AddMessage(role MessageRole, content string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := Message{
		ID:        uuid.NewString(),
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
	}
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = msg.Timestamp
	return msg
}

// AddMessageWithAction is like AddMessage but also attaches an Action to the
// stored message. It is used to record the action that triggered a state
// transition (e.g. an approval).
func (s *Session) AddMessageWithAction(role MessageRole, content string, action *Action) Message {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg := Message{
		ID:        uuid.NewString(),
		Role:      role,
		Content:   content,
		Timestamp: time.Now(),
		Action:    action,
	}
	s.Messages = append(s.Messages, msg)
	s.UpdatedAt = msg.Timestamp
	return msg
}

// History returns a copy of the message history. The returned slice is safe
// to mutate without affecting the session.
func (s *Session) History() []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Message, len(s.Messages))
	copy(out, s.Messages)
	return out
}

// SetState updates the session state and refreshes UpdatedAt.
func (s *Session) SetState(state SessionState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.State = state
	s.UpdatedAt = time.Now()
}

// GetState returns the current session state.
func (s *Session) GetState() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State
}

// SetRecommendation stores a recommendation on the session and refreshes
// UpdatedAt.
func (s *Session) SetRecommendation(rec *recommend.Recommendation) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Recommendation = rec
	s.UpdatedAt = time.Now()
}

// GetRecommendation returns the current recommendation or nil.
func (s *Session) GetRecommendation() *recommend.Recommendation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Recommendation
}

// SetWorkflowID records the id of the workflow currently executing.
func (s *Session) SetWorkflowID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WorkflowID = id
	s.UpdatedAt = time.Now()
}

// IsTerminal reports whether the session is in a terminal state (Done or
// Failed), i.e. no further state transitions are possible except /restart.
func (s *Session) IsTerminal() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.State == StateDone || s.State == StateFailed
}

// Reset returns the session to StateIdle and clears the recommendation and
// workflow id. The message history is preserved so the operator can still
// review the prior conversation.
func (s *Session) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.State = StateIdle
	s.Recommendation = nil
	s.WorkflowID = ""
	s.UpdatedAt = time.Now()
}

// --- helpers ----------------------------------------------------------------

// normalizeText trims surrounding whitespace and collapses internal runs so
// command matching is robust against extra spaces typed by the operator.
func normalizeText(s string) string {
	return strings.TrimSpace(s)
}

// hasPrefix reports whether s starts with the given command prefix followed
// by either end-of-string or whitespace. This avoids "/help" matching
// "/helpful" etc.
func hasPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := s[len(prefix):]
	return rest == "" || strings.HasPrefix(rest, " ")
}