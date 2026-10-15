package wiring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The engine side of gate-verdict tracing is tested against a fake recorder, and
// internal/audit tests RecordGate against a real store. Neither of those proves
// the seam in between: that the production constructor hands the runner a NON-NIL
// recorder at all. NewClosureRunner has exactly one call site (run.go), and the
// option it passes is built by gateRecorderOption — so "a run really carries
// per-gate rows" is a claim about this file, not about the engine.
//
// This test runs the full closure over the loopback transport with a workflow that
// declares a post_batch gate, then reads the trace rows back out of the same store
// the recorder writes through. Delete gateRecorderOption(store) from run.go and
// this goes red.

const gateSeamWorkflowYAML = `name: gate-seam
target:
  type: host
  query: "env=test"
steps:
- name: noop
  action: shell.exec
  args:
    cmd: noop-command
gates:
- position: post_batch
  cmd:
    run: "gate-ok"
    expect_exit: 0
`

func gateTraceRows(t *testing.T, store state.Store, runID string) []*state.Trace {
	t.Helper()
	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: runID})
	require.NoError(t, err)
	var rows []*state.Trace
	for _, tr := range traces {
		if tr.Event == audit.EventGateCheck {
			rows = append(rows, tr)
		}
	}
	return rows
}

func TestRunChange_GateVerdictsLandInRealTraceStore(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-gate-seam", gateSeamWorkflowYAML)
	planAndPersist(t, e, store, "run-gate-seam", []string{"web-1"})
	setRunStatus(t, store, "run-gate-seam", "running")

	_, success, _, err := e.runChange(context.Background(), "run-gate-seam", false, 1)
	require.NoError(t, err)
	require.True(t, success, "the gated run must succeed before its verdict rows mean anything")

	rows := gateTraceRows(t, store, "run-gate-seam")
	require.NotEmpty(t, rows, "a gated run must carry gate_check trace rows")

	for _, tr := range rows {
		assert.Equal(t, "system", tr.Actor)
		var detail struct {
			Target   string         `json:"target"`
			Output   map[string]any `json:"output"`
			Metadata map[string]any `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(tr.Detail), &detail), "detail must stay parseable")
		assert.Equal(t, "*", detail.Target)
		assert.Equal(t, true, detail.Output["passed"], "the loopback gate exits 0")
		assert.Equal(t, "post_batch", detail.Metadata["phase"])
		assert.NotEmpty(t, detail.Metadata["gate_name"],
			"the row must name the gate that produced the verdict")
		assert.NotEmpty(t, detail.Output["exec_run_id"],
			"re-keying onto the change id must not lose the engine's own run id")
	}

	// Gate rows are ordinary chain members: SealRunTraceChain runs right after
	// each insert, so an unsealable row would show up as a broken chain rather
	// than as a silent log line.
	b, err := audit.NewHashChainBuilder(store)
	require.NoError(t, err)
	sealed, err := b.Verify(context.Background(), "run-gate-seam")
	require.NoError(t, err, "the trace chain must still verify with gate rows in it")
	assert.GreaterOrEqual(t, sealed, len(rows))
}

func TestRunChange_UngatedRunCarriesNoGateRows(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-no-gate", execWorkflowYAML)
	planAndPersist(t, e, store, "run-no-gate", []string{"web-1"})
	setRunStatus(t, store, "run-no-gate", "running")

	_, success, _, err := e.runChange(context.Background(), "run-no-gate", false, 1)
	require.NoError(t, err)
	require.True(t, success)

	assert.Empty(t, gateTraceRows(t, store, "run-no-gate"),
		"no declared gates means no verdict rows, not rows asserting a pass")
	// This empty result is only evidence because the gated run above proves rows
	// CAN land: an ungated run legitimately writes no trace rows at all today
	// (the engine path records nothing else — see rollback_grade.go), so the
	// assertion would also pass in a build where tracing is dead entirely.
}
