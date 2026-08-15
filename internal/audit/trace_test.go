package audit

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore opens a fresh SQLiteStore backed by a temp file. Each test gets
// its own file so tests can run in parallel without colliding.
func newTestStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "audit_test.db")
	store, err := state.NewSQLiteStore(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newRecorder builds a TraceRecorder on top of a fresh temp-file store.
func newRecorder(t *testing.T) *TraceRecorder {
	t.Helper()
	store := newTestStore(t)
	rec, err := NewTraceRecorder(store)
	require.NoError(t, err)
	return rec
}

// createRun inserts a minimal run row so that trace records can satisfy the
// trace.run_id → runs.id foreign-key constraint.
func createRun(t *testing.T, store state.Store, runID string) {
	t.Helper()
	now := time.Now().UTC()
	err := store.CreateRun(context.Background(), &state.Run{
		ID:           runID,
		WorkflowName: "wf-test",
		TemplateName: "tpl-test",
		Status:       "running",
		CreatedAt:    now,
		UpdatedAt:    now,
		Creator:      "tester",
	})
	require.NoError(t, err)
}

// parseDetail unmarshals the JSON Detail column of a state.Trace into a
// traceDetail struct for assertion.
func parseDetail(t *testing.T, raw string) traceDetail {
	t.Helper()
	var d traceDetail
	require.NoError(t, json.Unmarshal([]byte(raw), &d))
	return d
}

func TestNewTraceRecorder_NilStore(t *testing.T) {
	rec, err := NewTraceRecorder(nil)
	require.ErrorIs(t, err, ErrNilStore)
	assert.Nil(t, rec)
}

func TestRecord_Basic(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-1")

	trace, err := rec.Record(ctx, TraceRecord{
		RunID:    "run-1",
		Event:    EventStepExecute,
		Actor:    "system",
		Target:   "host:node-1",
		Input:    map[string]any{"cmd": "ls -la"},
		Output:   map[string]any{"exit_code": 0},
		Duration: 150 * time.Millisecond,
		Metadata: map[string]string{"step_name": "list", "action": "shell"},
	})
	require.NoError(t, err)
	require.NotNil(t, trace)

	// ID should be a 32-char hex string.
	assert.Len(t, trace.ID, 32)
	assert.Equal(t, "run-1", trace.RunID)
	assert.Equal(t, EventStepExecute, trace.Event)
	assert.Equal(t, "system", trace.Actor)
	assert.NotEmpty(t, trace.Detail)
	assert.False(t, trace.Timestamp.IsZero())

	// Verify the persisted row matches.
	got, err := rec.store.GetTrace(ctx, trace.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, trace.ID, got.ID)
	assert.Equal(t, "run-1", got.RunID)

	d := parseDetail(t, got.Detail)
	assert.Equal(t, "host:node-1", d.Target)
	assert.Equal(t, map[string]any{"cmd": "ls -la"}, d.Input)
	assert.Equal(t, map[string]any{"exit_code": float64(0)}, d.Output)
	assert.EqualValues(t, 150, d.Duration)
	assert.Equal(t, "list", d.Metadata["step_name"])
	assert.Equal(t, "shell", d.Metadata["action"])
	assert.Empty(t, d.Error)
}

func TestRecord_EmptyRunID(t *testing.T) {
	rec := newRecorder(t)
	_, err := rec.Record(context.Background(), TraceRecord{
		Event: EventStepExecute,
	})
	require.ErrorIs(t, err, ErrEmptyRunID)
}

func TestRecord_EmptyEvent(t *testing.T) {
	rec := newRecorder(t)
	_, err := rec.Record(context.Background(), TraceRecord{
		RunID: "run-1",
	})
	require.ErrorIs(t, err, ErrEmptyEvent)
}

func TestRecord_ErrorField(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-err")

	trace, err := rec.Record(ctx, TraceRecord{
		RunID:    "run-err",
		Event:    EventStepExecute,
		Actor:    "system",
		Target:   "host:node-1",
		Duration: 10 * time.Millisecond,
		Error:    errors.New("connection refused"),
	})
	require.NoError(t, err)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, "connection refused", d.Error)
}

func TestRecord_EmptyInputOutput(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-empty")

	trace, err := rec.Record(ctx, TraceRecord{
		RunID:  "run-empty",
		Event:  EventPauseRun,
		Actor:  "alice",
		Target: "*",
	})
	require.NoError(t, err)

	d := parseDetail(t, trace.Detail)
	assert.Nil(t, d.Input)
	assert.Nil(t, d.Output)
	assert.Equal(t, "*", d.Target)
}

func TestRecordStep(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-1")

	trace, err := rec.RecordStep(ctx, "run-1", "host:node-2", "deploy", "rsync",
		map[string]any{"src": "/tmp/build"},
		map[string]any{"bytes": 4096},
		250*time.Millisecond,
		nil)
	require.NoError(t, err)
	assert.Equal(t, EventStepExecute, trace.Event)
	assert.Equal(t, "system", trace.Actor)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, "host:node-2", d.Target)
	assert.Equal(t, "deploy", d.Metadata["step_name"])
	assert.Equal(t, "rsync", d.Metadata["action"])
	assert.Equal(t, map[string]any{"src": "/tmp/build"}, d.Input)
	assert.Equal(t, map[string]any{"bytes": float64(4096)}, d.Output)
	assert.EqualValues(t, 250, d.Duration)
	assert.Empty(t, d.Error)
}

func TestRecordGate(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-1")

	trace, err := rec.RecordGate(ctx, "run-1", "pre-check", "before", true,
		map[string]any{"checked_hosts": 3})
	require.NoError(t, err)
	assert.Equal(t, EventGateCheck, trace.Event)
	assert.Equal(t, "system", trace.Actor)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, "*", d.Target)
	assert.Equal(t, "pre-check", d.Metadata["gate_name"])
	assert.Equal(t, "before", d.Metadata["phase"])
	assert.Equal(t, true, d.Output["passed"])
	assert.Equal(t, "before", d.Output["phase"])
	assert.EqualValues(t, 3, d.Output["checked_hosts"])
}

func TestRecordApproval(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-1")

	trace, err := rec.RecordApproval(ctx, "run-1", "L1", "alice", "approved", "lgtm")
	require.NoError(t, err)
	assert.Equal(t, EventApprovalDecision, trace.Event)
	assert.Equal(t, "alice", trace.Actor)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, "*", d.Target)
	assert.Equal(t, "L1", d.Metadata["level"])
	assert.Equal(t, "approved", d.Metadata["decision"])
	assert.Equal(t, "L1", d.Output["level"])
	assert.Equal(t, "approved", d.Output["decision"])
	assert.Equal(t, "lgtm", d.Output["comment"])
}

func TestListByRun_Ordering(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-order")
	createRun(t, rec.store, "run-other")

	// Record three traces for the same run with small sleeps so their
	// timestamps are strictly increasing. SQLite stores timestamps with
	// microsecond precision, so 1ms sleeps are more than enough.
	for i, ev := range []string{EventStepExecute, EventGateCheck, EventApprovalDecision} {
		trace, err := rec.Record(ctx, TraceRecord{
			RunID:  "run-order",
			Event:  ev,
			Actor:  "system",
			Target: "*",
			Metadata: map[string]string{
				"seq": string(rune('a' + i)),
			},
		})
		require.NoError(t, err)
		require.NotNil(t, trace)
		time.Sleep(2 * time.Millisecond)
	}

	// Record a trace for a different run to ensure it is not returned.
	_, err := rec.Record(ctx, TraceRecord{
		RunID: "run-other",
		Event: EventStepExecute,
	})
	require.NoError(t, err)

	got, err := rec.ListByRun(ctx, "run-order")
	require.NoError(t, err)
	require.Len(t, got, 3)

	// ListTraces orders by timestamp ASC, so the events should appear in the
	// order they were recorded.
	assert.Equal(t, EventStepExecute, got[0].Event)
	assert.Equal(t, EventGateCheck, got[1].Event)
	assert.Equal(t, EventApprovalDecision, got[2].Event)

	// Sanity check: timestamps are non-decreasing.
	assert.True(t, !got[1].Timestamp.Before(got[0].Timestamp))
	assert.True(t, !got[2].Timestamp.Before(got[1].Timestamp))
}

func TestListByRun_Empty(t *testing.T) {
	rec := newRecorder(t)
	got, err := rec.ListByRun(context.Background(), "run-none")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestRedact_TopLevel(t *testing.T) {
	in := map[string]any{
		"username": "alice",
		"password": "s3cr3t",
		"token":    "abc123",
		"safe":     42,
	}
	out := Redact(in)

	assert.Equal(t, "alice", out["username"])
	assert.Equal(t, redactedValue, out["password"])
	assert.Equal(t, redactedValue, out["token"])
	assert.Equal(t, 42, out["safe"])

	// Original input must not be mutated.
	assert.Equal(t, "s3cr3t", in["password"])
}

func TestRedact_Nested(t *testing.T) {
	in := map[string]any{
		"connection": map[string]any{
			"host":     "node-1",
			"password": "nested-secret",
			"tls": map[string]any{
				"private_key": "-----BEGIN-----",
				"ca":          "ca-bundle",
			},
		},
		"api_key": "top-level-key",
	}
	out := Redact(in)

	assert.Equal(t, redactedValue, out["api_key"])

	conn, ok := out["connection"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "node-1", conn["host"])
	assert.Equal(t, redactedValue, conn["password"])

	tls, ok := conn["tls"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedValue, tls["private_key"])
	assert.Equal(t, "ca-bundle", tls["ca"])
}

func TestRedact_CaseInsensitive(t *testing.T) {
	in := map[string]any{
		"Password": "P",
		"TOKEN":    "T",
		"Api_Key":  "K",
	}
	out := Redact(in)
	assert.Equal(t, redactedValue, out["Password"])
	assert.Equal(t, redactedValue, out["TOKEN"])
	assert.Equal(t, redactedValue, out["Api_Key"])
}

func TestRedact_NilInput(t *testing.T) {
	assert.Nil(t, Redact(nil))
}

func TestRedact_AllSensitiveFields(t *testing.T) {
	in := map[string]any{
		"password":    "1",
		"passwd":      "2",
		"key":         "3",
		"token":       "4",
		"secret":      "5",
		"credential":  "6",
		"private_key": "7",
		"api_key":     "8",
	}
	out := Redact(in)
	for k := range in {
		assert.Equal(t, redactedValue, out[k], "field %q should be redacted", k)
	}
}

func TestRecord_RedbackactsInputOutput(t *testing.T) {
	rec := newRecorder(t)
	ctx := context.Background()
	createRun(t, rec.store, "run-redact")

	trace, err := rec.Record(ctx, TraceRecord{
		RunID:  "run-redact",
		Event:  EventStepExecute,
		Actor:  "system",
		Target: "host:node-1",
		Input: map[string]any{
			"cmd":      "ssh",
			"password": "should-not-leak",
		},
		Output: map[string]any{
			"stdout": "ok",
			"token":  "should-not-leak",
		},
	})
	require.NoError(t, err)

	d := parseDetail(t, trace.Detail)
	assert.Equal(t, redactedValue, d.Input["password"])
	assert.Equal(t, "ssh", d.Input["cmd"])
	assert.Equal(t, redactedValue, d.Output["token"])
	assert.Equal(t, "ok", d.Output["stdout"])

	// The raw Detail JSON must not contain the plaintext secret.
	assert.NotContains(t, trace.Detail, "should-not-leak")
}
