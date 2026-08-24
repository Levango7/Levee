// Package audit records audit traces for every action executed by the LEVEE
// engine (step execution, gate checks, approval decisions, rollbacks, lock
// acquire/release, pause/resume, ...). Each action produces one TraceRecord
// that is persisted to the underlying state.Store. Sensitive fields (password,
// token, secret, ...) are redacted before persistence so that credentials
// never enter the trace chain.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/state"
)

// Event types recorded by the audit trace. Callers may also use custom event
// names; these constants exist to keep the well-known events consistent across
// the codebase.
const (
	EventStepExecute      = "step_execute"
	EventGateCheck        = "gate_check"
	EventApprovalDecision = "approval_decision"
	EventRollbackStep     = "rollback_step"
	EventLockAcquire      = "lock_acquire"
	EventLockRelease      = "lock_release"
	EventPauseRun         = "pause_run"
	EventResumeRun        = "resume_run"
)

// Sentinel errors. Callers can use errors.Is to distinguish failure modes.
var (
	// ErrNilStore is returned when a TraceRecorder is constructed with a nil
	// store.
	ErrNilStore = errors.New("audit: nil store")
	// ErrEmptyRunID is returned when a trace record is recorded without a run
	// id.
	ErrEmptyRunID = errors.New("audit: empty run id")
	// ErrEmptyEvent is returned when a trace record is recorded without an
	// event type.
	ErrEmptyEvent = errors.New("audit: empty event")
)

// sensitiveFields is the set of field names whose values are replaced with
// "[REDACTED]" by Redact. Matching is case-insensitive.
var sensitiveFields = map[string]struct{}{
	"password":    {},
	"passwd":      {},
	"key":         {},
	"token":       {},
	"secret":      {},
	"credential":  {},
	"private_key": {},
	"api_key":     {},
}

// redactedValue is the placeholder substituted for sensitive field values.
const redactedValue = "[REDACTED]"

// TraceRecorder records audit traces. Each action produces one TraceRecord
// that is persisted to state.Store. TraceRecorder is stateless beyond the
// store reference; the hash chain (T044) is built on top of the persisted
// records.
type TraceRecorder struct {
	store state.Store
}

// TraceRecord is the input parameter for recording one audit trace entry. The
// recorder fills in the persistent fields (ID, Timestamp) and serialises
// Input/Output/Metadata into the Detail JSON column of state.Trace.
type TraceRecord struct {
	RunID    string            // associated run id
	Event    string            // event type, see Event* constants
	Actor    string            // who performed the action (user or "system")
	Target   string            // target host ("host:xxx") or "*" for global
	Input    map[string]any    // action input parameters
	Output   map[string]any    // action output results
	Duration time.Duration     // how long the action took
	Error    error             // error returned by the action, if any
	Metadata map[string]string // extra metadata (batch_no, step_name, ...)
}

// traceDetail is the JSON payload stored in state.Trace.Detail. It captures
// the full context of an action so that the audit chain can be replayed.
type traceDetail struct {
	Target   string            `json:"target,omitempty"`
	Input    map[string]any    `json:"input,omitempty"`
	Output   map[string]any    `json:"output,omitempty"`
	Duration int64             `json:"duration_ms"`
	Error    string            `json:"error,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// NewTraceRecorder creates a TraceRecorder backed by the given store. The
// store must be non-nil.
func NewTraceRecorder(store state.Store) (*TraceRecorder, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	return &TraceRecorder{store: store}, nil
}

// Record persists one trace entry. It generates a unique id, redacts sensitive
// fields in Input/Output, serialises the detail payload to JSON and inserts
// the resulting state.Trace. The returned *state.Trace is the row that was
// written.
//
// Record does not set PrevHash/CurrHash; the hash chain is built by T044.
func (r *TraceRecorder) Record(ctx context.Context, record TraceRecord) (*state.Trace, error) {
	if record.RunID == "" {
		return nil, ErrEmptyRunID
	}
	if record.Event == "" {
		return nil, ErrEmptyEvent
	}

	id, err := newID()
	if err != nil {
		return nil, fmt.Errorf("audit: generate trace id: %w", err)
	}

	detail, err := buildDetail(record)
	if err != nil {
		return nil, fmt.Errorf("audit: build trace detail: %w", err)
	}

	trace := &state.Trace{
		ID:        id,
		RunID:     record.RunID,
		Event:     record.Event,
		Actor:     record.Actor,
		Detail:    detail,
		Timestamp: time.Now().UTC(),
	}

	if err := r.store.CreateTrace(ctx, trace); err != nil {
		return nil, fmt.Errorf("audit: create trace: %w", err)
	}
	return trace, nil
}

// RecordStep is a convenience wrapper for recording a step execution. It sets
// Event to EventStepExecute, Target to the given target host, Actor to
// "system" (steps are executed by the engine, not a human), and populates
// Input/Output/Metadata with the step-specific fields.
func (r *TraceRecorder) RecordStep(
	ctx context.Context,
	runID, target, stepName, action string,
	input, output map[string]any,
	duration time.Duration,
	err error,
) (*state.Trace, error) {
	return r.Record(ctx, TraceRecord{
		RunID:    runID,
		Event:    EventStepExecute,
		Actor:    "system",
		Target:   target,
		Input:    input,
		Output:   output,
		Duration: duration,
		Error:    err,
		Metadata: map[string]string{
			"step_name": stepName,
			"action":    action,
		},
	})
}

// RecordGate is a convenience wrapper for recording a gate check. It sets
// Event to EventGateCheck, Target to "*" (gates are global per run), Actor to
// "system", and encodes the gate name, phase and pass/fail result in Metadata
// and Output.
func (r *TraceRecorder) RecordGate(
	ctx context.Context,
	runID, gateName, phase string,
	passed bool,
	detail map[string]any,
) (*state.Trace, error) {
	output := make(map[string]any, len(detail)+2)
	for k, v := range detail {
		output[k] = v
	}
	output["passed"] = passed
	output["phase"] = phase

	return r.Record(ctx, TraceRecord{
		RunID:  runID,
		Event:  EventGateCheck,
		Actor:  "system",
		Target: "*",
		Output: output,
		Metadata: map[string]string{
			"gate_name": gateName,
			"phase":     phase,
		},
	})
}

// RecordApproval is a convenience wrapper for recording an approval decision.
// It sets Event to EventApprovalDecision, Target to "*" (approvals are not
// host-scoped), Actor to the approver, and encodes the level/decision/comment
// in Metadata and Output.
func (r *TraceRecorder) RecordApproval(
	ctx context.Context,
	runID, level, approver, decision, comment string,
) (*state.Trace, error) {
	return r.Record(ctx, TraceRecord{
		RunID:  runID,
		Event:  EventApprovalDecision,
		Actor:  approver,
		Target: "*",
		Output: map[string]any{
			"level":    level,
			"decision": decision,
			"comment":  comment,
		},
		Metadata: map[string]string{
			"level":    level,
			"decision": decision,
		},
	})
}

// ListByRun returns all trace records for the given run, ordered by timestamp
// ascending (i.e. in the order they were recorded). A nil slice is returned
// when the run has no traces.
func (r *TraceRecorder) ListByRun(ctx context.Context, runID string) ([]*state.Trace, error) {
	traces, err := r.store.ListTraces(ctx, state.TraceFilter{RunID: runID})
	if err != nil {
		return nil, fmt.Errorf("audit: list traces by run %q: %w", runID, err)
	}
	return traces, nil
}

// Redact returns a copy of input with every sensitive field replaced by
// "[REDACTED]". Matching is case-insensitive and recursive: nested maps,
// string-keyed maps and slices are walked and redacted. Non-composite values
// are returned unchanged.
//
// Redact never mutates the input map; it always returns a fresh map (or the
// original value when input is not a map).
func Redact(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = redactValueForKey(k, v)
	}
	return out
}

// RedactStringMap returns a copy of m with every sensitive key's value
// replaced by "[REDACTED]". It exists because trace details carry string
// maps too (e.g. TraceRecord.Metadata), which previously bypassed redaction
// entirely. Matching is case-insensitive; a nil input yields nil.
func RedactStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if isSensitive(k) {
			out[k] = redactedValue
			continue
		}
		out[k] = v
	}
	return out
}

// redactValueForKey redacts a single value given its key. If the key is
// sensitive the value is replaced with "[REDACTED]"; otherwise the value is
// recursively redacted when it is a composite (map or slice).
func redactValueForKey(key string, value any) any {
	if isSensitive(key) {
		return redactedValue
	}
	return redactComposite(value)
}

// redactComposite recurses into maps and slices without a key context (e.g.
// elements of a JSON array). Values of other types are returned unchanged.
func redactComposite(value any) any {
	switch v := value.(type) {
	case map[string]any:
		return Redact(v)
	case map[string]string:
		return RedactStringMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactComposite(item)
		}
		return out
	default:
		return value
	}
}

// isSensitive reports whether key names a sensitive field. Matching is
// case-insensitive.
func isSensitive(key string) bool {
	_, ok := sensitiveFields[strings.ToLower(key)]
	return ok
}

// buildDetail serialises the TraceRecord into the JSON string stored in
// state.Trace.Detail. Sensitive fields in Input/Output/Metadata are redacted
// before serialisation so credentials never reach the trace chain.
func buildDetail(record TraceRecord) (string, error) {
	d := traceDetail{
		Target:   record.Target,
		Input:    Redact(record.Input),
		Output:   Redact(record.Output),
		Duration: record.Duration.Milliseconds(),
		Metadata: RedactStringMap(record.Metadata),
	}
	if record.Error != nil {
		d.Error = record.Error.Error()
	}

	buf, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("marshal detail: %w", err)
	}
	return string(buf), nil
}

// newID generates a 16-byte hex-encoded random id (32 chars). It uses
// crypto/rand so that ids are unpredictable and collision-resistant.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
