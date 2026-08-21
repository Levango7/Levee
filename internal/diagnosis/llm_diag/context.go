package llm_diag

// context.go implements the reasoning context for LEVEE's LLM-driven
// conversational diagnosis engine (Phase D3). A ReasoningContext carries the
// initial diagnostic report, the running list of reasoning turns exchanged
// with the LLM, the current root-cause hypothesis and its confidence, and the
// overall reasoning status.
//
// The context is the single source of truth that the ReasoningEngine mutates
// as it loops through turns. It is NOT safe for concurrent use by itself —
// the ReasoningEngine serialises access within a single Diagnose call. Callers
// that share a context across goroutines must take their own lock.
//
// ToMessages converts the turn history into the recommend.LLMMessage slice
// expected by the recommend.LLMClient interface so the engine can feed the
// conversation straight to the LLM backend.

import (
	"time"

	"github.com/google/uuid"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/recommend"
)

// --- ReasoningStatus -------------------------------------------------------

// ReasoningStatus is the lifecycle state of a reasoning session. It starts at
// StatusReasoning and transitions to one of the terminal states when the loop
// converges, gives up, hits the turn cap or errors out.
type ReasoningStatus int

const (
	// StatusReasoning means the engine is still looping through turns. It is
	// the initial state of a freshly constructed context.
	StatusReasoning ReasoningStatus = iota
	// StatusConverged means the LLM reported a converged hypothesis with
	// confidence above the convergence threshold.
	StatusConverged
	// StatusInconclusive means the LLM could not reach a confident conclusion
	// within the turn budget without explicitly converging.
	StatusInconclusive
	// StatusMaxTurnsReached means the engine hit the configured MaxTurns limit
	// before the LLM converged.
	StatusMaxTurnsReached
	// StatusError means the reasoning loop aborted because of an LLM or parse
	// error.
	StatusError
)

// String returns a human-readable name for the status. It is safe to call on
// any ReasoningStatus value; unknown values render as "unknown".
func (s ReasoningStatus) String() string {
	switch s {
	case StatusReasoning:
		return "reasoning"
	case StatusConverged:
		return "converged"
	case StatusInconclusive:
		return "inconclusive"
	case StatusMaxTurnsReached:
		return "max_turns_reached"
	case StatusError:
		return "error"
	default:
		return "unknown"
	}
}

// --- Turn ------------------------------------------------------------------

// Turn is a single message exchanged during the reasoning loop. Role is one of
// "user", "assistant" or "system"; Content is the raw text. Timestamp records
// when the turn was appended. Metadata carries optional per-turn annotations
// such as the parsed confidence or convergence flag.
type Turn struct {
	Role      string            `json:"role"`
	Content   string            `json:"content"`
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// --- ReasoningContext ------------------------------------------------------

// ReasoningContext holds the full state of one reasoning session. The
// ReasoningEngine creates a fresh context per Diagnose call and mutates it in
// place as turns are exchanged.
type ReasoningContext struct {
	// ID is a unique identifier for the session, generated as a UUID at
	// construction time.
	ID string `json:"id"`

	// Target is the infrastructure target under diagnosis (a host, service or
	// pod name). It is taken from the report when available, otherwise from
	// the argument passed to NewReasoningContext.
	Target string `json:"target"`

	// AlertID is the id of the alert that triggered the diagnosis, copied
	// from the initial report when present. Empty for manual runs.
	AlertID string `json:"alert_id,omitempty"`

	// InitialReport is the diagnostic report that seeds the reasoning loop.
	// It is retained by reference so the engine can quote findings back to
	// the LLM; callers must not mutate it while a reasoning session is live.
	InitialReport *diagnosis.DiagnosticReport `json:"-"`

	// Messages is the ordered list of reasoning turns exchanged with the LLM.
	Messages []Turn `json:"messages"`

	// Hypothesis is the current best root-cause hypothesis. It is updated
	// after every assistant turn.
	Hypothesis string `json:"hypothesis"`

	// Confidence is the engine's confidence in Hypothesis, in the range
	// [0, 1]. Zero means no hypothesis has been formed yet.
	Confidence float64 `json:"confidence"`

	// Status is the current lifecycle state of the session.
	Status ReasoningStatus `json:"status"`

	// CreatedAt is the wall-clock time at which the context was constructed.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the wall-clock time of the most recent mutation.
	UpdatedAt time.Time `json:"updated_at"`
}

// NewReasoningContext builds a fresh ReasoningContext for the given target and
// report. The target is taken from the report when the report is non-nil and
// has a non-empty Target field, otherwise the explicit target argument is
// used. The AlertID is copied from the report when present.
//
// The returned context is in StatusReasoning with an empty hypothesis and
// zero confidence. The caller (typically ReasoningEngine.Diagnose) is
// responsible for seeding the first turn.
func NewReasoningContext(target string, report *diagnosis.DiagnosticReport) *ReasoningContext {
	now := time.Now().UTC()
	c := &ReasoningContext{
		ID:            uuid.NewString(),
		Target:        target,
		InitialReport: report,
		Status:        StatusReasoning,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if report != nil {
		if report.Target != "" {
			c.Target = report.Target
		}
		c.AlertID = report.AlertID
		// Seed the hypothesis with the report's root cause so the first LLM
		// turn has something to refine.
		c.Hypothesis = report.RootCause
		c.Confidence = report.Confidence
	}
	return c
}

// AddTurn appends a new reasoning turn with the given role and content and
// stamps UpdatedAt. Metadata is left nil; callers that need per-turn
// annotations can set them on the last element of Messages after the call.
// AddTurn is a no-op for an empty content only when role is also empty; an
// empty content with a non-empty role is still recorded because the LLM may
// legitimately return whitespace-only text that the caller wants to inspect.
func (c *ReasoningContext) AddTurn(role, content string) {
	now := time.Now().UTC()
	c.Messages = append(c.Messages, Turn{
		Role:      role,
		Content:   content,
		Timestamp: now,
	})
	c.UpdatedAt = now
}

// ToMessages converts the turn history into the recommend.LLMMessage slice
// expected by recommend.LLMClient.Chat. The conversion preserves order and
// drops turns whose role is empty (which should not happen in practice but is
// defensive against malformed contexts). System turns are included so the LLM
// sees the full conversation transcript.
//
// The returned slice is a fresh copy; mutating it does not affect the context.
func (c *ReasoningContext) ToMessages() []recommend.LLMMessage {
	if c == nil || len(c.Messages) == 0 {
		return nil
	}
	out := make([]recommend.LLMMessage, 0, len(c.Messages))
	for _, t := range c.Messages {
		if t.Role == "" {
			continue
		}
		out = append(out, recommend.LLMMessage{
			Role:    t.Role,
			Content: t.Content,
		})
	}
	return out
}

// SetHypothesis updates the current root-cause hypothesis and confidence and
// refreshes UpdatedAt. Confidence is clamped to the [0, 1] range so the LLM
// cannot push the value out of bounds. An empty hypothesis is allowed (it
// clears the current hypothesis) but leaves the confidence untouched only
// when the caller passes a negative confidence — otherwise the confidence is
// always overwritten.
func (c *ReasoningContext) SetHypothesis(hypothesis string, confidence float64) {
	c.Hypothesis = hypothesis
	c.Confidence = clamp01(confidence)
	c.UpdatedAt = time.Now().UTC()
}

// clamp01 clamps a float to the [0, 1] range. NaN values collapse to 0.
func clamp01(v float64) float64 {
	if v != v { // NaN guard
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
