// End-to-end coverage for the two surfaces that read the evidence verdict:
// `levee show` (a run's record rendered honestly) and `levee retry` (the
// decision to re-run over an undocumented state, recorded on the run).
package main

import (
	"context"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func traceEventsOfRun(t *testing.T, e *cliEnv, runID string) []string {
	t.Helper()
	store := e.open(t)
	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	out := make([]string, 0, len(traces))
	for _, tr := range traces {
		out = append(out, tr.Event)
	}
	return out
}

func seedBatchRow(t *testing.T, e *cliEnv, runID, batchID string) {
	t.Helper()
	store := e.open(t)
	now := time.Now().UTC()
	require.NoError(t, store.CreateBatch(context.Background(), &state.Batch{
		ID: batchID, RunID: runID, BatchNo: 1, Status: state.BatchStateCompleted,
		TotalHosts: 1, Succeeded: 1, StartedAt: &now, CompletedAt: &now,
	}))
}

// TestShowE2E_EvidenceVerdict: an interrupted run with no evidence rows must
// render as "missing — may have changed hosts", not as an empty step list.
func TestShowE2E_EvidenceVerdict(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	e.seedRun(t, "run-int", "interrupted")
	e.seedRun(t, "run-ok", "completed")
	seedBatchRow(t, e, "run-ok", "bat-ok")

	out := mustRun(t, append([]string{"show", "run-int"}, cfg...)...)
	assert.Contains(t, out, "Evidence:")
	assert.Contains(t, out, "missing")
	assert.Contains(t, out, "may have changed hosts",
		"the verdict must say what is unknown, not just that a row is absent")

	res, _, err := freshJSON(t, append([]string{"show", "run-int", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok := res["data"].(map[string]any)
	require.True(t, ok)
	ev, ok := data["evidence"].(map[string]any)
	require.True(t, ok, "the JSON envelope must carry the verdict: %v", data)
	assert.Equal(t, "missing", ev["kind"])
	assert.Equal(t, true, ev["unsafe_to_retry"])

	// Control: a run whose batches were persisted reads as complete, so the
	// warning has to be something the verdict decided, not a fixed line.
	res, _, err = freshJSON(t, append([]string{"show", "run-ok", "--json"}, cfg...)...)
	require.NoError(t, err)
	data, ok = res["data"].(map[string]any)
	require.True(t, ok)
	ev, ok = data["evidence"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "complete", ev["kind"])
	assert.Equal(t, false, ev["unsafe_to_retry"])
}

// TestRetryE2E_UndocumentedOutcomeIsRecorded: the CLI retry of a failed run
// whose evidence never landed must leave a retry_over_unknown_outcome trace —
// the operator was warned, and the run's own chain says the decision was taken.
func TestRetryE2E_UndocumentedOutcomeIsRecorded(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	e.seedRun(t, "run-blind", "failed")

	mustRun(t, append([]string{"retry", "run-blind"}, cfg...)...)
	assert.Contains(t, traceEventsOfRun(t, e, "run-blind"), state.EventRetryOverUnknownOutcome)
}

// TestRetryE2E_DocumentedOutcomeIsNotFlagged is the control: a run whose
// evidence is complete must not gain the event, or the event becomes noise and
// the operator learns to ignore the one that matters.
func TestRetryE2E_DocumentedOutcomeIsNotFlagged(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	cfg := []string{"--config", e.cfgPath}
	e.seedRun(t, "run-doc", "failed")
	seedBatchRow(t, e, "run-doc", "bat-doc")

	mustRun(t, append([]string{"retry", "run-doc"}, cfg...)...)
	assert.NotContains(t, traceEventsOfRun(t, e, "run-doc"), state.EventRetryOverUnknownOutcome)
}
