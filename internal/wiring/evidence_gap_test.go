// evidence_gap_test.go covers the two executor-side routes that leave a run
// whose evidence table does not describe what happened. Both are "we lost the
// record" paths, and the whole point of internal/evidence is that the loss is
// RECORDED rather than inferred from an empty table — so each test asserts the
// trace row AND the verdict a reader derives from it.
package wiring

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/evidence"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// traceEventsOf returns the run's trace event names in recording order.
func traceEventsOf(t *testing.T, store state.Store, runID string) []string {
	t.Helper()
	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	out := make([]string, 0, len(traces))
	for _, tr := range traces {
		out = append(out, tr.Event)
	}
	return out
}

// failingStepStore fails CreateStep on demand: the "execution finished, writing
// the evidence did not" route. Everything else goes to the real store.
type failingStepStore struct {
	state.Store
	failSteps bool
}

func (s *failingStepStore) CreateStep(ctx context.Context, st *state.Step) error {
	if s.failSteps {
		return errors.New("evidence store unavailable (test)")
	}
	return s.Store.CreateStep(ctx, st)
}

// TestExecutePlan_PersistFailureIsRecordedAsIncomplete: the steps ran, the
// batch rows landed, and the step rows did not. Without the event this run
// reads as fully documented (batches exist, so the verdict would be "complete")
// — which is the false-reassurance shape the feature exists to remove.
func TestExecutePlan_PersistFailureIsRecordedAsIncomplete(t *testing.T) {
	rec := &loopRecorder{}
	reg := channel.NewChannelRegistry()
	reg.Register("local", loopFactory{rec: rec})
	_, base := newTestEngine(t)
	store := &failingStepStore{Store: base, failSteps: true}
	e := NewEngine(store, WithChannelRegistry(reg))

	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-pf", execWorkflowYAML)
	planAndPersist(t, e, store, "run-pf", []string{"web-1"})
	setRunStatus(t, store, "run-pf", "running")

	_, success, phase, err := e.runChange(context.Background(), "run-pf", false, 1)
	require.Error(t, err, "the persist failure must still surface to the caller")
	assert.Contains(t, err.Error(), "persisting step evidence")
	assert.False(t, success)
	assert.Equal(t, "completed", phase, "the execution itself succeeded; only the record did not")

	assert.Equal(t, 1, len(rec.snapshotCmds()), "the step really ran")
	assert.Empty(t, stepsOf(t, store, "run-pf"))

	events := traceEventsOf(t, store, "run-pf")
	assert.Contains(t, events, state.EventEvidenceIncomplete,
		"the gap must be written down, not inferred from a missing row")

	batches, err := store.ListBatches(context.Background(), state.BatchFilter{RunID: "run-pf"})
	require.NoError(t, err)
	verdict := evidence.Classify(evidence.Facts{
		Status:  state.BatchStateCompleted, // the status the caller will settle on
		Events:  events,
		Batches: len(batches),
		Steps:   len(stepsOf(t, store, "run-pf")),
	})
	assert.Equal(t, evidence.Partial, verdict.Kind,
		"batch rows exist, so the roster alone would claim completeness")
}

// TestExecutePlan_FencedOutExecutionRecordsTheDiscard: the executor lost the
// lease AFTER executing and its evidence is dropped on purpose. The run is not
// "empty" — it is undocumented, and the trace has to say so.
func TestExecutePlan_FencedOutExecutionRecordsTheDiscard(t *testing.T) {
	rec := &loopRecorder{}
	g := &fakeGuard{flipAfterOwns: 1, ownsErr: errors.New("cluster: own-check for \"run-fd\": fenced out")}
	e, store := newLoopEngine(t, rec, WithExecutionGuard(g, "node-x"), WithExecLeaseTTL(90*time.Second))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-fd", execWorkflowYAML)
	planAndPersist(t, e, store, "run-fd", []string{"web-1"})
	setRunStatus(t, store, "run-fd", "running")

	_, _, _, err := e.runChange(context.Background(), "run-fd", false, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced out before evidence persistence")
	assert.Empty(t, stepsOf(t, store, "run-fd"))

	events := traceEventsOf(t, store, "run-fd")
	assert.Contains(t, events, state.EventEvidenceDiscarded)

	verdict := evidence.Classify(evidence.Facts{
		Status: "interrupted", // what the takeover settles this run to
		Events: events,
	})
	assert.Equal(t, evidence.Missing, verdict.Kind)
	assert.True(t, verdict.Kind.UnsafeToBlindRetry())
}

// TestEvidenceGapTraceSurvivesACancelledContext: both routes can fire while the
// process is being torn down. A gap recorded under the caller's (cancelled)
// context would vanish exactly when it matters most.
func TestEvidenceGapTraceSurvivesACancelledContext(t *testing.T) {
	_, store := newTestEngine(t)
	e := NewEngine(store)
	seedRun(t, store, "run-cx", execWorkflowYAML)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.recordRunFact(ctx, "run-cx", state.EventEvidenceIncomplete, map[string]any{"reason": "test"})

	assert.Contains(t, traceEventsOf(t, store, "run-cx"), state.EventEvidenceIncomplete)
}

// TestRetryChange_RecordsAnUndocumentedOutcome: the engine re-runs a plan whose
// previous attempt left no step evidence. That decision goes on the run's trace
// chain, so "we re-applied this over an unknown state" is a fact an auditor can
// read rather than a thing the operator was supposed to remember.
func TestRetryChange_RecordsAnUndocumentedOutcome(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-blind", execWorkflowYAML)
	planAndPersist(t, e, store, "run-blind", []string{"web-1"})
	setRunStatus(t, store, "run-blind", "failed") // planned, failed, never executed

	require.NoError(t, e.retryChange(context.Background(), "run-blind", false, []string{"web-1"}))

	events := traceEventsOf(t, store, "run-blind")
	assert.Contains(t, events, state.EventRetryOverUnknownOutcome,
		"the retry of an undocumented run must be recorded on its chain")
	assert.Contains(t, events, "retry_finished", "the control trace must still be written")
}

// TestRetryChange_DocumentedOutcomeIsNotFlagged is the control: a run whose
// steps were persisted must not gain the event, or the warning becomes noise
// and the real one is ignored.
func TestRetryChange_DocumentedOutcomeIsNotFlagged(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-doc", execWorkflowYAML)
	planAndPersist(t, e, store, "run-doc", []string{"web-1"})

	_, _, _, err := e.runChange(context.Background(), "run-doc", false, 1)
	require.NoError(t, err)
	setRunStatus(t, store, "run-doc", "failed")

	require.NoError(t, e.retryChange(context.Background(), "run-doc", false, []string{"web-1"}))
	assert.NotContains(t, traceEventsOf(t, store, "run-doc"), state.EventRetryOverUnknownOutcome)
}
