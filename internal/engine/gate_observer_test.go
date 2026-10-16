package engine

// gate_observer_test.go covers the second sink for gate verdicts: the typed
// observer that feeds chat broadcast (#18's remaining "EventGateResult has zero
// senders" gap). The properties worth pinning are the ones that make the two
// sinks worth having together:
//   - audit row and broadcast describe the SAME verdicts (one source, so they
//     cannot disagree about which gates ran or what each decided);
//   - the observer is orthogonal to the run, exactly like the recorder — with
//     no recorder installed it still fires, and installing it changes no
//     verdict, in either direction (green stays green, red still rolls back).

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/verify"
)

type observingGate struct {
	mu   sync.Mutex
	seen []GateVerdict
}

func (o *observingGate) observe(_ context.Context, v GateVerdict) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, v)
}

func (o *observingGate) snapshot() []GateVerdict {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]GateVerdict, len(o.seen))
	copy(out, o.seen)
	return out
}

// newGateFixtureRunner builds a runner with the usual test dependencies plus
// the given gates, and returns it with a recorder so a test can install or omit
// the second sink without touching the other.
func newGateFixtureRunner(t *testing.T, withRecorder bool, gates ...verify.Gate) (*ClosureRunner, *recordingGateRecorder) {
	t.Helper()
	store := newTestStore(t)
	cr := newTestClosureRunner(t, store, gates...)
	rec := &recordingGateRecorder{}
	if withRecorder {
		cr.gateRecorder = rec
	}
	return cr, rec
}

func twoBatchGreenPlanGates() []verify.Gate {
	return []verify.Gate{
		verify.NewNoopGate("pre-a", verify.PhasePreApply, true),
		verify.NewNoopGate("post-batch-ok", verify.PhasePostBatch, true),
		verify.NewNoopGate("post-apply-ok", verify.PhasePostApply, true),
	}
}

// --- 1. both sinks describe the same verdicts ------------------------------

func TestGateVerdicts_ObserverAgreesWithAuditSink(t *testing.T) {
	cr, rec := newGateFixtureRunner(t, true, twoBatchGreenPlanGates()...)
	obs := &observingGate{}
	cr.gateObserver = obs.observe

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
	require.NoError(t, err)
	require.Equal(t, PhaseCompleted, result.Phase)

	rows := rec.snapshot()
	seen := obs.snapshot()
	require.Len(t, seen, len(rows), "one notification per recorded verdict")
	require.Len(t, seen, 4, "pre_apply(1) + post_batch(1 gate x 2 batches) + post_apply(1)")

	for i := range rows {
		assert.Equal(t, rows[i].gateName, seen[i].GateName)
		assert.Equal(t, rows[i].phase, seen[i].Phase)
		assert.Equal(t, rows[i].passed, seen[i].Passed)
		assert.Equal(t, rows[i].runID, seen[i].ExecRunID,
			"the observer carries the engine id as ExecRunID, so nothing downstream mistakes it for the change id")
		assert.Empty(t, seen[i].ChangeID,
			"the engine cannot know the change id; wiring binds it (changeBoundGateObserver)")
		assert.NotEmpty(t, seen[i].Message, "a verdict with no message would render an empty summary line")
	}

	// Batch scoping survives onto the typed value, not just the map.
	batches := map[string]bool{}
	for _, v := range seen {
		if v.Phase == "post_batch" {
			batches[v.BatchID] = true
		}
	}
	assert.Equal(t, map[string]bool{"batch-0": true, "batch-1": true}, batches)
}

// --- 2. the two sinks are independent -------------------------------------

// TestGateVerdicts_ObserverFiresWithoutRecorder: an earlier shape of this code
// returned early on a nil recorder, so "audit disabled" would have silently
// become "chat disabled" too. The sinks are separate decisions, and this is the
// test that keeps them that way.
func TestGateVerdicts_ObserverFiresWithoutRecorder(t *testing.T) {
	cr, rec := newGateFixtureRunner(t, false, twoBatchGreenPlanGates()...)
	cr.gateRecorder = nil
	obs := &observingGate{}
	cr.gateObserver = obs.observe

	p := newTestPlan([][]string{{"host-a"}, {"host-b"}})
	result, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
	require.NoError(t, err)
	assert.Equal(t, PhaseCompleted, result.Phase)
	assert.Empty(t, rec.snapshot(), "no recorder, no rows")
	assert.Len(t, obs.snapshot(), 4, "a deployment that records nothing still announces")
}

// TestGateVerdicts_FailingGateStillRollsBackWithObserverInstalled: the
// announcement must not soften a failure. The baseline (no observer) and the
// observed run have to agree verdict-for-verdict.
func TestGateVerdicts_FailingGateStillRollsBackWithObserverInstalled(t *testing.T) {
	build := func(withObserver bool) (*ClosureResult, []GateVerdict) {
		cr, _ := newGateFixtureRunner(t, true,
			verify.NewNoopGate("pre-ok", verify.PhasePreApply, true),
			verify.NewNoopGate("post-apply-bad", verify.PhasePostApply, false),
		)
		obs := &observingGate{}
		if withObserver {
			cr.gateObserver = obs.observe
		}
		p := newTestPlan([][]string{{"host-a"}})
		res, err := cr.Run(context.Background(), p, (&mockExecutor{}).exec)
		require.NoError(t, err)
		return res, obs.snapshot()
	}

	baseRes, baseSeen := build(false)
	withRes, withSeen := build(true)

	assert.Equal(t, baseRes.Phase, withRes.Phase, "installing a broadcast sink cannot change the run's outcome")
	require.NotEmpty(t, withSeen, "the failing run must still produce notifications")
	for _, v := range withSeen {
		if v.GateName == "post-apply-bad" {
			assert.False(t, v.Passed, "the failure is announced as a failure")
		}
	}
	// The baseline ran the same gates and reached the same verdicts — compared
	// through the engine's own result list, NOT through baseSeen, which is empty
	// by definition when no observer is installed (asserting on that would have
	// been a tautology dressed as a control).
	assert.Empty(t, baseSeen, "no observer means no notifications, not silent ones")
	require.Len(t, baseRes.VerifyResults, len(withSeen),
		"every verdict the engine acted on is announced exactly once")
	for i := range baseRes.VerifyResults {
		assert.Equal(t, baseRes.VerifyResults[i].Passed, withSeen[i].Passed)
	}
}

// --- 3. the audit row's shape is the one #154 shipped ---------------------

// TestGateVerdictAuditDetailKeepsRowShape: the engine used to build this map
// inline. It is now derived from the typed verdict, so the row a reader (and
// the console timeline) sees must be byte-for-byte what it was — including the
// zero-value OMISSIONS: no stage key when the producer had none, no batch key
// outside post_batch.
func TestGateVerdictAuditDetailKeepsRowShape(t *testing.T) {
	base := GateVerdict{Message: "ok", Latency: 1500 * time.Millisecond}

	got := base.AuditDetail()
	assert.Equal(t, map[string]any{"message": "ok", "latency_ms": int64(1500)}, got,
		"the default row is message + latency and nothing else")

	staged := base
	staged.Stage = "rollback"
	assert.Equal(t, "rollback", staged.AuditDetail()["stage"])

	batched := base
	batched.BatchID = "batch-2"
	assert.Equal(t, "batch-2", batched.AuditDetail()["batch"])

	both := staged
	both.BatchID = "batch-0"
	assert.Len(t, both.AuditDetail(), 4, "stage and batch are independent keys")

	// A zero latency still appears (as 0), because the key's presence is the
	// contract the timeline reads — dropping it for a fast gate would make the
	// row look like a different shape.
	fast := base
	fast.Latency = 0
	assert.Equal(t, int64(0), fast.AuditDetail()["latency_ms"])
}

// --- 4. naming is derived from the manager that ran the gates --------------

func TestGateVerdicts_MappingNamesAndFallback(t *testing.T) {
	gm := verify.NewGateManager()
	gm.Register(verify.NewNoopGate("named", verify.PhasePreApply, true))

	results := []verify.GateResult{{Passed: true, Message: "m", Latency: 2 * time.Millisecond}}
	vs := gateVerdicts("run-x", gm, verify.PhasePreApply, "forward", "", results)
	require.Len(t, vs, 1)
	assert.Equal(t, "named", vs[0].GateName)
	assert.Equal(t, "run-x", vs[0].ExecRunID)
	assert.Equal(t, "forward", vs[0].Stage)
	assert.Empty(t, vs[0].BatchID)

	// More results than the manager has gates for that phase: the index guard
	// degrades to a positional name rather than dropping the verdict, because a
	// positional label is still evidence while silence is not.
	orphan := gateVerdicts("run-x", gm, verify.PhasePreApply, "", "batch-9",
		[]verify.GateResult{{Passed: false, Message: "m"}, {Passed: true, Message: "m"}})
	require.Len(t, orphan, 2)
	assert.Equal(t, "named", orphan[0].GateName)
	assert.Equal(t, "gate-1", orphan[1].GateName, "out-of-range index falls back to a positional name, not to a skip")
	assert.Equal(t, "batch-9", orphan[1].BatchID)

	assert.Nil(t, gateVerdicts("run-x", gm, verify.PhasePreApply, "", "", nil),
		"no results means no verdicts, so neither sink is invoked")
}
