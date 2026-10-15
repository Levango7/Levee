package engine

// gate_verdict_trace_test.go covers the engine writing one audit trace row per
// verification-gate verdict (the #18 "gate verdicts" gap: the writer
// audit.TraceRecorder.RecordGate existed with zero production callers, so a run
// carried no record per gate despite hashchain.go's comment claiming it did).
//
// The properties worth pinning are exactly the ones that were live risks, not
// "a row appears":
//   - gate NAMES are recovered by index from Gates(phase), so RunPhase's
//     positional-alignment contract must hold across MULTIPLE same-phase gates
//     (one gate per phase would pass even if alignment broke);
//   - recording is orthogonal to the verdict: a nil recorder, and a recorder
//     that ERRORS, must leave the run's outcome byte-for-byte the same;
//   - the post-rollback health check shares the post_apply phase token with the
//     forward check, so it must be distinguishable (stage="rollback"), and its
//     trace row must be produced by the code path actually running — not assumed.

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

type recordedGate struct {
	runID    string
	gateName string
	phase    string
	passed   bool
	detail   map[string]any
}

// recordingGateRecorder is a fake GateTraceRecorder. fail makes RecordGate
// return an error so the "write failure must not flip the verdict" contract is
// exercised rather than merely commented.
type recordingGateRecorder struct {
	mu   sync.Mutex
	rows []recordedGate
	fail bool
}

func (r *recordingGateRecorder) RecordGate(_ context.Context, runID, gateName, phase string, passed bool, detail map[string]any) (*state.Trace, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, recordedGate{runID: runID, gateName: gateName, phase: phase, passed: passed, detail: detail})
	if r.fail {
		return nil, assert.AnError
	}
	return &state.Trace{RunID: runID, Event: "gate_check"}, nil
}

func (r *recordingGateRecorder) snapshot() []recordedGate {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedGate, len(r.rows))
	copy(out, r.rows)
	return out
}

// --- 1. multi-gate index alignment + per-phase recording ------------------

func TestGateVerdicts_RecordedPerPhase(t *testing.T) {
	store := newTestStore(t)
	rec := &recordingGateRecorder{}
	// Two gates on the SAME phase (pre_apply) with distinct names: this is what
	// makes the index-alignment assertion load-bearing. A single gate per phase
	// would pass even if Gates(phase)[i] no longer lined up with results[i].
	gates := []verify.Gate{
		verify.NewNoopGate("pre-a", verify.PhasePreApply, true),
		verify.NewNoopGate("pre-b", verify.PhasePreApply, true),
		verify.NewNoopGate("post-batch-ok", verify.PhasePostBatch, true),
		verify.NewNoopGate("post-apply-ok", verify.PhasePostApply, true),
	}
	cr := newTestClosureRunner(t, store, gates...)
	cr.gateRecorder = rec

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, result.Phase)

	rows := rec.snapshot()
	// pre_apply(2) + post_batch(1 gate x 2 batches) + post_apply(1) = 5.
	assert.Len(t, rows, 5, "one row per gate result across all phases")
	assert.Len(t, result.VerifyResults, 5, "rows must match the gate results the engine acted on")

	// Every verdict passed and recorded as passed.
	for _, r := range rows {
		assert.True(t, r.passed, "gate %s/%s should be recorded passing", r.phase, r.gateName)
		assert.Equal(t, "forward", r.detail["stage"], "forward-path verdicts are stage=forward")
		assert.NotEmpty(t, r.runID, "every row is attributed to the run being executed")
	}

	// Names recovered by index must be the gate's own name, per phase.
	preNames := namesForPhase(rows, "pre_apply")
	assert.ElementsMatch(t, []string{"pre-a", "pre-b"}, preNames,
		"both same-phase gates named correctly (index alignment holds)")
	assert.ElementsMatch(t, []string{"post-batch-ok", "post-batch-ok"}, namesForPhase(rows, "post_batch"))
	assert.ElementsMatch(t, []string{"post-apply-ok"}, namesForPhase(rows, "post_apply"))

	// The post-batch rows carry the batch id they were evaluated for.
	batches := map[string]bool{}
	for _, r := range rows {
		if r.phase == "post_batch" {
			b, _ := r.detail["batch"].(string)
			batches[b] = true
		}
	}
	assert.Equal(t, map[string]bool{"batch-0": true, "batch-1": true}, batches,
		"each batch's gates are attributed to that batch")
}

// --- 2. nil recorder: verdict identical, no rows --------------------------

func TestGateVerdicts_NilRecorderKeepsVerdict(t *testing.T) {
	store := newTestStore(t)
	// newTestClosureRunner installs NO recorder → gateRecorder is nil.
	cr := newTestClosureRunner(t, store,
		verify.NewNoopGate("pre-ok", verify.PhasePreApply, true),
		verify.NewNoopGate("post-apply-ok", verify.PhasePostApply, true))

	p := newTestPlan([][]string{{"host-a"}})
	result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase, "a deployment that wired no recorder completes identically")
	assert.Len(t, result.VerifyResults, 2, "the gates still run; only recording is off")
}

// --- 3. write failure must not flip any verdict ---------------------------

func TestGateVerdicts_WriteFailureDoesNotFlipVerdict(t *testing.T) {
	t.Run("green stays green", func(t *testing.T) {
		store := newTestStore(t)
		rec := &recordingGateRecorder{fail: true}
		cr := newTestClosureRunner(t, store, verify.NewNoopGate("pre-ok", verify.PhasePreApply, true))
		cr.gateRecorder = rec
		p := newTestPlan([][]string{{"host-a"}})
		result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
		require.NoError(t, err)
		assert.Equal(t, PhaseCompleted, result.Phase, "a store write error must not turn a passing gate into a failure")
		assert.NotEmpty(t, rec.snapshot(), "the recorder was still invoked (failure is in the write, not the call)")
	})

	t.Run("red stays red", func(t *testing.T) {
		store := newTestStore(t)
		rec := &recordingGateRecorder{fail: true}
		cr := newTestClosureRunner(t, store, verify.NewNoopGate("pre-fail", verify.PhasePreApply, false))
		cr.gateRecorder = rec
		p := newTestPlan([][]string{{"host-a"}})
		result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
		require.Error(t, err)
		assert.Equal(t, PhaseFailed, result.Phase, "a failing gate still fails even when its record could not be written")
	})
}

// --- 4. post-rollback verdicts are recorded under stage=rollback ----------

func TestGateVerdicts_PostRollbackIsTaggedRollback(t *testing.T) {
	store := newTestStore(t)
	rec := &recordingGateRecorder{}
	// A failing forward post_apply gate forces a rollback; verify_after runs the
	// post-rollback health gate (also PhasePostApply). Both share the phase
	// token, so only the stage tag keeps them apart in the trail.
	gmGates := []verify.Gate{verify.NewNoopGate("post-apply-fail", verify.PhasePostApply, false)}
	pvGates := []verify.Gate{verify.NewNoopGate("post-rollback-health", verify.PhasePostApply, true)}
	cr := newRunnerWithPostVerifier(t, store, gmGates, pvGates)
	cr.gateRecorder = rec

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	p.Rollback = &dsl.RollbackSpec{VerifyAfter: true}
	result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
	require.NoError(t, err)
	require.Equal(t, PhaseRolledBack, result.Phase, "the failing forward gate must trigger a rollback")
	require.NotNil(t, result.PostVerifyResult, "verify_after opted in")

	rows := rec.snapshot()
	var forward, rollback []recordedGate
	for _, r := range rows {
		switch r.detail["stage"] {
		case "forward":
			forward = append(forward, r)
		case "rollback":
			rollback = append(rollback, r)
		}
	}
	// The forward failing post_apply check is recorded (passed=false).
	require.Len(t, forward, 1)
	assert.Equal(t, "post-apply-fail", forward[0].gateName)
	assert.False(t, forward[0].passed)
	assert.Equal(t, "post_apply", forward[0].phase)

	// The post-rollback health check is recorded too, same phase token but
	// stage=rollback and passed=true — proving the two are distinguishable.
	require.Len(t, rollback, 1)
	assert.Equal(t, "post-rollback-health", rollback[0].gateName)
	assert.True(t, rollback[0].passed)
	assert.Equal(t, "post_apply", rollback[0].phase,
		"1b: the rollback check keeps the post_apply phase token and is told apart by stage=rollback")
}

// --- 5. fragile precondition guard ----------------------------------------

// TestPostRollbackVerifyUsesPhaseMode pins that the engine calls
// VerifyAndGrade in PHASE mode (verifyGates == nil). The stage=rollback rows
// name their gates by index into Gates(PhasePostApply); that is only sound
// while the verifier ran exactly that phase's gate list in that order. If the
// call ever switches to a named subset, results come back in caller order and
// the positional name lookup would silently mislabel which gate produced which
// verdict. Same "scan the call site" idiom the calendar/rollback guards use.
func TestPostRollbackVerifyUsesPhaseMode(t *testing.T) {
	src, err := os.ReadFile("closure.go")
	require.NoError(t, err, "the engine source must be readable from its package dir")
	line := findLine(string(src), "postVerifier.VerifyAndGrade(")
	require.NotEmpty(t, line, "the post-rollback verify call site must exist")
	assert.Contains(t, line, ", nil, ",
		"VerifyAndGrade must keep verifyGates=nil (phase mode); a named list breaks the positional gate naming")
}

// --- helpers --------------------------------------------------------------

func namesForPhase(rows []recordedGate, phase string) []string {
	var out []string
	for _, r := range rows {
		if r.phase == phase {
			out = append(out, r.gateName)
		}
	}
	return out
}

func findLine(src, needle string) string {
	for _, l := range strings.Split(src, "\n") {
		if strings.Contains(l, needle) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}
