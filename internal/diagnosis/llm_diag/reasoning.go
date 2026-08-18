
package llm_diag

// reasoning.go implements Phase D3 of LEVEE's diagnostic subsystem: a
// multi-turn reasoning engine that drives an LLM to converge on a root-cause
// hypothesis through conversational refinement.
//
// The ReasoningEngine takes an initial DiagnosticReport and runs a loop of at
// most MaxTurns LLM calls. On each turn it sends the running transcript to the
// LLM along with a system prompt that frames the diagnostic task, parses the
// JSON response (hypothesis / confidence / converged / suggestions), updates
// the ReasoningContext, and stops early when the LLM reports convergence or
// the confidence crosses the convergence threshold.
//
// Every prompt and message is run through the recommend.Sanitizer before being
// sent so that credentials, IP addresses and other sensitive data in the
// diagnostic report never leave the process. The engine never panics; all
// failures are propagated through error returns.
//
// The engine is safe for concurrent use: it carries no mutable state of its
// own, and each Diagnose call operates on a fresh ReasoningContext.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/recommend"
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrNilLLM is returned by NewReasoningEngine when the config does not
	// supply an LLMClient. The reasoning engine cannot operate without one.
	ErrNilLLM = errors.New("llm_diag: nil llm client")
	// ErrNilReport is returned by Diagnose when the report argument is nil.
	ErrNilReport = errors.New("llm_diag: nil diagnostic report")
	// ErrEmptyResponse is returned when the LLM returns an empty string that
	// cannot be parsed into a reasoning response.
	ErrEmptyResponse = errors.New("llm_diag: empty llm response")
	// ErrParseResponse is returned when the LLM response is not valid JSON
	// in the expected shape.
	ErrParseResponse = errors.New("llm_diag: parse llm response")
)

// --- Defaults ---------------------------------------------------------------

const (
	// DefaultMaxTurns is the turn cap applied when ReasoningEngineConfig.MaxTurns
	// is zero or negative.
	DefaultMaxTurns = 5
	// DefaultConvergenceThreshold is the confidence above which the engine
	// treats a hypothesis as converged even when the LLM does not set
	// "converged": true.
	DefaultConvergenceThreshold = 0.8
	// DefaultReasoningTimeout is applied when the caller does not set a
	// context deadline.
	DefaultReasoningTimeout = 2 * time.Minute
	// reasoningTemperature is the sampling temperature used for the LLM call.
	// Low temperature keeps the reasoning deterministic.
	reasoningTemperature = 0.2
)

// --- ReasoningEngineConfig --------------------------------------------------

// ReasoningEngineConfig configures a ReasoningEngine. LLM is the only
// mandatory field; the rest fall back to sensible defaults.
type ReasoningEngineConfig struct {
	// LLM is the LLM client used for every reasoning turn. Must be non-nil.
	LLM recommend.LLMClient

	// Sanitizer scrubs sensitive data before it is sent to the LLM. When nil
	// the engine uses recommend.NewSanitizer() with the default rule set.
	Sanitizer *recommend.Sanitizer

	// MaxTurns is the maximum number of LLM calls in a single Diagnose run.
	// Zero or negative values fall back to DefaultMaxTurns.
	MaxTurns int

	// ConvergenceThreshold is the confidence above which the engine stops
	// early. Zero falls back to DefaultConvergenceThreshold.
	ConvergenceThreshold float64

	// Logger is the structured logger. When nil the engine uses a logger
	// tagged with component="llm_diag_engine".
	Logger *slog.Logger
}

// --- ReasoningEngine --------------------------------------------------------

// ReasoningEngine drives the multi-turn LLM reasoning loop. It is safe for
// concurrent use: each Diagnose call works on its own ReasoningContext.
type ReasoningEngine struct {
	llm                 recommend.LLMClient
	sanitizer           *recommend.Sanitizer
	maxTurns            int
	convergenceThreshold float64
	log                 *slog.Logger
}

// NewReasoningEngine builds a ReasoningEngine from the config. It returns
// ErrNilLLM when cfg.LLM is nil — the engine cannot operate without an LLM.
func NewReasoningEngine(cfg ReasoningEngineConfig) (*ReasoningEngine, error) {
	if cfg.LLM == nil {
		return nil, ErrNilLLM
	}

	sanitizer := cfg.Sanitizer
	if sanitizer == nil {
		sanitizer = recommend.NewSanitizer()
	}

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	threshold := cfg.ConvergenceThreshold
	if threshold <= 0 {
		threshold = DefaultConvergenceThreshold
	}

	lg := cfg.Logger
	if lg == nil {
		lg = log.With("component", "llm_diag_engine")
	}

	return &ReasoningEngine{
		llm:                 cfg.LLM,
		sanitizer:           sanitizer,
		maxTurns:            maxTurns,
		convergenceThreshold: threshold,
		log:                 lg,
	}, nil
}

// --- ReasoningResult --------------------------------------------------------

// ReasoningResult is the outcome of a single Diagnose call. It wraps the final
// ReasoningContext and surfaces the key fields (hypothesis, confidence, root
// cause, suggestions, turn count, status, duration) for quick inspection.
type ReasoningResult struct {
	// Context is the final reasoning context, including the full transcript.
	Context *ReasoningContext `json:"context"`

	// Hypothesis is the final root-cause hypothesis. It mirrors
	// Context.Hypothesis.
	Hypothesis string `json:"hypothesis"`

	// Confidence is the engine's confidence in Hypothesis, in [0, 1].
	Confidence float64 `json:"confidence"`

	// RootCause is the final root cause. For the reasoning engine the root
	// cause is the converged hypothesis; when the loop did not converge it
	// is the best hypothesis so far.
	RootCause string `json:"root_cause"`

	// Suggestions is the list of remediation suggestions from the last LLM
	// turn. Empty when the LLM did not provide any.
	Suggestions []string `json:"suggestions"`

	// Turns is the number of LLM calls actually performed.
	Turns int `json:"turns"`

	// Status is the final reasoning status.
	Status ReasoningStatus `json:"status"`

	// Duration is the wall-clock time spent in the reasoning loop.
	Duration time.Duration `json:"duration"`
}

// --- Diagnose ---------------------------------------------------------------

// Diagnose runs the multi-turn reasoning loop for the given target and report.
// It returns ErrNilReport when report is nil. On an LLM or parse error the
// engine returns a non-nil ReasoningResult (with Status == StatusError and the
// turns completed so far) together with the error, so callers can inspect the
// partial transcript.
func (e *ReasoningEngine) Diagnose(ctx context.Context, target string, report *diagnosis.DiagnosticReport) (*ReasoningResult, error) {
	if report == nil {
		return nil, ErrNilReport
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Apply a default timeout when the caller has not set a deadline so a
	// misbehaving LLM cannot hang the diagnosis forever.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultReasoningTimeout)
		defer cancel()
	}

	start := time.Now()
	rctx := NewReasoningContext(target, report)
	systemPrompt := buildSystemPrompt(rctx.Target, report)

	var suggestions []string
	turns := 0

	for turn := 1; turn <= e.maxTurns; turn++ {
		// 1. Build the user prompt for this turn and record it.
		userPrompt := buildTurnPrompt(turn, e.maxTurns, rctx.Hypothesis, rctx.Confidence)
		rctx.AddTurn("user", userPrompt)

		// 2. Sanitise the transcript and system prompt before sending.
		messages := e.sanitizer.SanitizeMessages(rctx.ToMessages())
		sanitisedSystem := e.sanitizer.Sanitize(systemPrompt)

		// 3. Call the LLM.
		resp, err := e.llm.ChatWithSystem(ctx, sanitisedSystem, messages)
		if err != nil {
			rctx.Status = StatusError
			e.log.Warn("llm_diag: llm call failed", "turn", turn, "err", err)
			return e.buildResult(rctx, suggestions, turns, start), fmt.Errorf("llm_diag: turn %d: %w", turn, err)
		}

		// 4. Record the raw assistant response.
		rctx.AddTurn("assistant", resp)
		turns++

		// 5. Parse the JSON response.
		parsed, perr := parseReasoningResponse(resp)
		if perr != nil {
			rctx.Status = StatusError
			e.log.Warn("llm_diag: parse response failed", "turn", turn, "err", perr)
			return e.buildResult(rctx, suggestions, turns, start), fmt.Errorf("llm_diag: turn %d: %w", turn, perr)
		}

		// 6. Update the hypothesis and track suggestions.
		rctx.SetHypothesis(parsed.Hypothesis, parsed.Confidence)
		if len(parsed.Suggestions) > 0 {
			suggestions = parsed.Suggestions
		}

		e.log.Debug("llm_diag: turn completed",
			"turn", turn,
			"confidence", parsed.Confidence,
			"converged", parsed.Converged,
		)

		// 7. Check for convergence.
		if parsed.Converged || parsed.Confidence >= e.convergenceThreshold {
			rctx.Status = StatusConverged
			break
		}
	}

	// If we exited the loop without converging, the turn cap was hit.
	if rctx.Status == StatusReasoning {
		rctx.Status = StatusMaxTurnsReached
	}

	return e.buildResult(rctx, suggestions, turns, start), nil
}

// buildResult assembles a ReasoningResult from the final context.
func (e *ReasoningEngine) buildResult(rctx *ReasoningContext, suggestions []string, turns int, start time.Time) *ReasoningResult {
	return &ReasoningResult{
		Context:     rctx,
		Hypothesis:  rctx.Hypothesis,
		Confidence:  rctx.Confidence,
		RootCause:   rctx.Hypothesis,
		Suggestions: suggestions,
		Turns:       turns,
		Status:      rctx.Status,
		Duration:    time.Since(start),
	}
}

// --- Prompt construction ----------------------------------------------------

// buildSystemPrompt constructs the system prompt that frames the diagnostic
// reasoning task. It summarises the target, findings, initial root cause and
// confidence from the report so the LLM has the full context on every turn.
func buildSystemPrompt(target string, report *diagnosis.DiagnosticReport) string {
	var b strings.Builder
	b.WriteString("You are a diagnostic reasoning engine for infrastructure incidents.\n")
	b.WriteString("Given the initial diagnostic report, reason about the root cause through multiple turns.\n\n")
	fmt.Fprintf(&b, "Target: %s\n", target)
	fmt.Fprintf(&b, "Initial findings: %s\n", formatFindings(report))
	fmt.Fprintf(&b, "Initial root cause hypothesis: %s\n", report.RootCause)
	fmt.Fprintf(&b, "Initial confidence: %.2f\n\n", report.Confidence)
	b.WriteString("For each turn, provide:\n")
	b.WriteString("1. Your current hypothesis about the root cause\n")
	b.WriteString("2. Your confidence level (0-1)\n")
	b.WriteString("3. Whether you have converged (true/false)\n")
	b.WriteString("4. Any additional suggestions\n\n")
	b.WriteString("Respond in JSON format:\n")
	b.WriteString(`{"hypothesis": "...", "confidence": 0.85, "converged": true, "suggestions": ["..."]}`)
	return b.String()
}

// buildTurnPrompt constructs the user message for a single reasoning turn. The
// prompt tells the LLM which turn it is on and reminds it of the current
// hypothesis so it can refine the analysis. The output is deterministic for a
// given (turn, maxTurns, hypothesis, confidence) tuple so tests can key mock
// responses off it.
func buildTurnPrompt(turn, maxTurns int, hypothesis string, confidence float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Turn %d of %d.\n", turn, maxTurns)
	fmt.Fprintf(&b, "Current hypothesis: %s\n", hypothesis)
	fmt.Fprintf(&b, "Current confidence: %.2f\n", confidence)
	b.WriteString("Analyze the diagnostic report and refine your root cause hypothesis.\n")
	b.WriteString("Respond in JSON: {\"hypothesis\": \"...\", \"confidence\": 0.0, \"converged\": false, \"suggestions\": []}.")
	return b.String()
}

// formatFindings renders the report findings as a compact, human-readable
// string for inclusion in the system prompt. Each finding is one line:
// "[severity] category: title — description". When the report has no findings
// the string "none" is returned so the prompt is never empty.
func formatFindings(report *diagnosis.DiagnosticReport) string {
	if report == nil || len(report.Findings) == 0 {
		return "none"
	}
	var b strings.Builder
	for i, f := range report.Findings {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "[%s] %s: %s", f.Severity, f.Category, f.Title)
		if f.Description != "" {
			b.WriteString(" — ")
			b.WriteString(f.Description)
		}
	}
	return b.String()
}

// --- Response parsing -------------------------------------------------------

// reasoningResponse is the JSON shape the LLM is asked to return on every turn.
type reasoningResponse struct {
	Hypothesis  string   `json:"hypothesis"`
	Confidence  float64  `json:"confidence"`
	Converged   bool     `json:"converged"`
	Suggestions []string `json:"suggestions"`
}

// parseReasoningResponse parses an LLM response into a reasoningResponse. The
// response may be bare JSON or a JSON block wrapped in markdown code fences;
// both forms are accepted. An empty response yields ErrEmptyResponse; invalid
// JSON yields ErrParseResponse.
func parseReasoningResponse(response string) (*reasoningResponse, error) {
	jsonStr := stripJSONFences(response)
	if strings.TrimSpace(jsonStr) == "" {
		return nil, ErrEmptyResponse
	}
	var r reasoningResponse
	if err := json.Unmarshal([]byte(jsonStr), &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseResponse, err)
	}
	return &r, nil
}

// stripJSONFences removes markdown code fences (```json ... ``` or ``` ... ```)
// from a response string. When no fences are present the input is returned
// trimmed. The function is tolerant of a missing language tag and a missing
// closing fence.
func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// Drop the opening fence line (and any language tag on it).
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	// Drop the closing fence if present.
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}