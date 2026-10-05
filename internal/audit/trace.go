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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
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
// the resulting state.Trace. It then seals the run's hash chain over the record
// just written, so the chain covers a run while it is still running rather than
// only once it settles. The returned *state.Trace is the row that was written.
//
// Record does not set PrevHash/CurrHash itself; the chain columns are written by
// the seal that follows the insert, because a row's position in the chain is
// only known once every earlier record is in place.
//
// A seal failure is logged, not returned: the trace itself is already durably
// written, and LEVEE treats audit-writing as non-fatal for the action that
// triggered it (the same contract as Record for audit entries). An unsealed row
// is not silently accepted — verification reports it as empty_hash and the next
// seal closes it.
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
	if err := SealRunTraceChain(ctx, r.store, trace.RunID); err != nil {
		log.Warn("audit: trace chain seal failed",
			"trace_id", trace.ID,
			"run_id", trace.RunID,
			"event", trace.Event,
			"error", err)
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

// idClock lets newID hand out strictly increasing time prefixes even when the
// wall clock did not advance between two calls.
var idClock = struct {
	sync.Mutex
	last int64
}{}

// newID returns a 32-character hex id: 8 bytes of big-endian unix time in
// nanoseconds, forced strictly greater than the previous issued value, followed
// by 8 crypto/rand bytes. So ids are unpredictable within the same nanosecond
// and globally sortable by creation time.
//
// The sortable half is not cosmetic. A run's trace chain is walked in
// (timestamp, id) order — see ListTraces — and Record seals that chain after
// every insert, so a record that lands *before* an already-sealed one is moved
// off the predecessor its hash was computed against. With purely random ids the
// tie-break is arbitrary: two records sharing a timestamp (routine on a clock
// with millisecond granularity — measured 2026-10-05, local Windows build with
// SQLite: 5 of the first 84 three-record loops ended with a chain that Verify
// reported broken) get sealed in an order that later flips, and the next seal
// reports an innocent run as tampered. Creation-ordered ids make the tie-break
// agree with the order records were appended in, so an append can only ever
// extend the tail.
//
// Two residual ways to reposition anyway are documented rather than fixed, and
// both keep the seal's refusal (rewriting instead would launder a genuine
// relink): a backwards wall-clock step between two appends, because the row's
// timestamp column goes backwards even though its id does not; and a late
// append to a run whose existing records were written by a binary that issued
// random ids. Either way Verify reports the break and BuildForce is the
// deliberate administrative repair.
func newID() (string, error) {
	ns := time.Now().UnixNano()
	idClock.Lock()
	if ns <= idClock.last {
		ns = idClock.last + 1
	}
	idClock.last = ns
	idClock.Unlock()

	b := make([]byte, 16)
	if _, err := rand.Read(b[8:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	binary.BigEndian.PutUint64(b[:8], uint64(ns))
	return hex.EncodeToString(b), nil
}
