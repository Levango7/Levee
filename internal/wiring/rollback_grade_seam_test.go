package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rollbackGradeWorkflowYAML = rbWorkflowYAML + `rollback:
  on_failure: auto
  verify_after: true
`

func TestRunChange_PartialRollbackGradeLandsInRealTraceChain(t *testing.T) {
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true}}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-grade", rollbackGradeWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-grade", []string{"web-1"})
	setRunStatus(t, store, "run-rb-grade", "running")

	execRunID, success, phase, err := e.runChange(context.Background(), "run-rb-grade", false, 1)
	require.NoError(t, err)
	assert.False(t, success)
	assert.Equal(t, "rolled_back_partial", phase)
	require.NotEmpty(t, execRunID)

	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: "run-rb-grade"})
	require.NoError(t, err)
	var grades []*state.Trace
	for _, tr := range traces {
		if tr.Event == "rollback_grade" {
			grades = append(grades, tr)
		}
	}
	require.Len(t, grades, 1, "a partial rollback must leave one searchable grade record")

	tr := grades[0]
	assert.Equal(t, "run-rb-grade", tr.RunID, "trace foreign keys must use the persisted change id")
	assert.Equal(t, "system", tr.Actor)
	var detail struct {
		Target   string         `json:"target"`
		Output   map[string]any `json:"output"`
		Metadata map[string]any `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal([]byte(tr.Detail), &detail))
	assert.Equal(t, "*", detail.Target)
	assert.Equal(t, string(rollback.GradePartial), detail.Output["grade"])
	assert.Equal(t, false, detail.Output["success"])
	assert.Equal(t, true, detail.Output["partial"])
	assert.Equal(t, float64(2), detail.Output["required_compensations"],
		"both dispatched forward steps require compensation; boom has none declared")
	assert.Equal(t, float64(1), detail.Output["completed_compensations"])
	assert.Equal(t, float64(1), detail.Output["unknown_side_effects"])
	assert.Contains(t, detail.Output["summary"], "rollback grade=partial")
	assert.Equal(t, execRunID, detail.Output["exec_run_id"])
	assert.Equal(t, string(rollback.GradePartial), detail.Metadata["grade"])

	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	sealed, err := b.Verify(context.Background(), "run-rb-grade")
	require.NoError(t, err, "the grade row must extend a verifiable trace chain")
	assert.GreaterOrEqual(t, sealed, len(traces))
}

func TestRunChange_FailedRollbackGradeLandsInRealTraceChain(t *testing.T) {
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true, "undo-command": true}}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-grade-failed", rollbackGradeWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-grade-failed", []string{"web-1"})
	setRunStatus(t, store, "run-rb-grade-failed", "running")

	execRunID, success, phase, err := e.runChange(context.Background(), "run-rb-grade-failed", false, 1)
	require.NoError(t, err)
	assert.False(t, success)
	assert.Equal(t, "rollback_incomplete", phase)

	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: "run-rb-grade-failed"})
	require.NoError(t, err)
	var grade *state.Trace
	for _, tr := range traces {
		if tr.Event == "rollback_grade" {
			require.Nil(t, grade, "one rollback must produce exactly one grade row")
			grade = tr
		}
	}
	require.NotNil(t, grade, "a failed rollback must leave a searchable grade record")
	assert.Equal(t, "system", grade.Actor)
	var detail struct {
		Output   map[string]any `json:"output"`
		Metadata map[string]any `json:"metadata"`
		Error    string         `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(grade.Detail), &detail))
	assert.Equal(t, string(rollback.GradeFailure), detail.Output["grade"])
	assert.Equal(t, false, detail.Output["partial"])
	assert.Equal(t, float64(2), detail.Output["required_compensations"])
	assert.Equal(t, float64(0), detail.Output["completed_compensations"])
	assert.Equal(t, execRunID, detail.Output["exec_run_id"])
	assert.Equal(t, string(rollback.GradeFailure), detail.Metadata["grade"])
	assert.Contains(t, detail.Error, "forced failure for undo-command")

	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	_, err = b.Verify(context.Background(), "run-rb-grade-failed")
	require.NoError(t, err)
}

func TestNewRunRollbackGraderSuccessWritesNoGradeAudit(t *testing.T) {
	store := newGradeTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.CreateRun(ctx, &state.Run{ID: "run-rb-success", WorkflowName: "wf"}))
	g := NewEngine(store).newRunRollbackGrader(nil, "run-rb-success")

	grade, err := g.GradeAndAct(ctx, &rollback.RollbackResult{RunID: "exec-success", Success: true})
	require.NoError(t, err)
	assert.Equal(t, rollback.GradeSuccess, grade)
	traces, err := store.ListTraces(ctx, state.TraceFilter{RunID: "run-rb-success"})
	require.NoError(t, err)
	for _, tr := range traces {
		assert.NotEqual(t, "rollback_grade", tr.Event, "success retains the no-audit-action contract")
	}
}

type failingGradeTraceStore struct {
	state.Store
	err      error
	attempts atomic.Int32
}

func (s *failingGradeTraceStore) CreateTrace(context.Context, *state.Trace) error {
	s.attempts.Add(1)
	return s.err
}

func TestNewRunRollbackGraderReportsAuditWriteFailure(t *testing.T) {
	boom := errors.New("trace store unavailable")
	store := &failingGradeTraceStore{Store: newGradeTestStore(t), err: boom}
	g := NewEngine(store).newRunRollbackGrader(nil, "change-audit")

	grade, err := g.GradeAndAct(context.Background(), &rollback.RollbackResult{
		RunID: "exec-audit", RequiredCompensations: 1,
	})
	assert.Equal(t, rollback.GradeFailure, grade)
	assert.ErrorIs(t, err, boom)
	assert.Equal(t, int32(1), store.attempts.Load())
}

func TestRunChange_AuditWriteFailureDoesNotRewriteRollbackVerdict(t *testing.T) {
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true}}
	e, store := newLoopEngine(t, rec)
	boom := errors.New("trace store unavailable")
	failing := &failingGradeTraceStore{Store: store, err: boom}
	e.store = failing
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb-audit-failed", rollbackGradeWorkflowYAML)
	planAndPersist(t, e, store, "run-rb-audit-failed", []string{"web-1"})
	setRunStatus(t, store, "run-rb-audit-failed", "running")

	_, success, phase, err := e.runChange(context.Background(), "run-rb-audit-failed", false, 1)
	require.NoError(t, err)
	assert.False(t, success)
	assert.Equal(t, "rolled_back_partial", phase)
	assert.Equal(t, int32(1), failing.attempts.Load(), "the failed audit write must actually be attempted")
}
