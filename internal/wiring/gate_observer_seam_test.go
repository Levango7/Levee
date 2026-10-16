package wiring

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/engine"
)

// gate_observer_seam_test.go pins the middle hop of the gate broadcast: that
// the production runner construction binds the CHANGE identity onto every
// verdict before the deployment's sink sees it.
//
// This is the same class of gap that made #154's first version write zero rows
// while every unit test stayed green: the engine hands out its own ephemeral id,
// and only wiring knows the persistent one. A sink that reads `ChangeID` would
// get "" in production and nothing in the engine's own tests would notice — so
// the assertion has to run through newRunRunner's call site, with a real plan
// and real gates.

type captureObserver struct {
	mu   sync.Mutex
	seen []engine.GateVerdict
}

func (c *captureObserver) observe(_ context.Context, v engine.GateVerdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, v)
}

func (c *captureObserver) snapshot() []engine.GateVerdict {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]engine.GateVerdict, len(c.seen))
	copy(out, c.seen)
	return out
}

func TestRunChange_GateVerdictReachesObserverWithChangeID(t *testing.T) {
	rec := &loopRecorder{}
	obs := &captureObserver{}
	e, store := newLoopEngine(t, rec, WithGateObserver(obs.observe))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-gate-observe", gateSeamWorkflowYAML)
	planAndPersist(t, e, store, "run-gate-observe", []string{"web-1"})
	setRunStatus(t, store, "run-gate-observe", "running")

	_, success, _, err := e.runChange(context.Background(), "run-gate-observe", false, 1)
	require.NoError(t, err)
	require.True(t, success, "the gated fixture passes its gate; a red run here means the fixture moved, not the seam")

	seen := obs.snapshot()
	require.NotEmpty(t, seen, "a run with a declared gate must notify the installed sink")
	for _, v := range seen {
		assert.Equal(t, "run-gate-observe", v.ChangeID,
			"the sink must receive the change id — this is what makes the card actionable")
		assert.NotEmpty(t, v.ExecRunID, "the engine's own id is kept alongside it, not instead of it")
		assert.NotEqual(t, v.ChangeID, v.ExecRunID,
			"they are different identities; collapsing them would re-create #154's foreign-key failure")
		assert.NotEmpty(t, v.GateName)
		assert.Equal(t, "post_batch", v.Phase, "the fixture declares one post_batch gate")
		assert.Equal(t, "forward", v.Stage)
		assert.True(t, v.Passed)
	}
}

// TestChangeBoundGateObserverNilStaysNil: a deployment with no sink must not end
// up with a non-nil observer, or "announcing is off" stops being observable from
// the outside — the same trap as wrapping a typed nil recorder in a non-nil
// interface.
func TestChangeBoundGateObserverNilStaysNil(t *testing.T) {
	assert.Nil(t, changeBoundGateObserver("chg-1", nil))

	var called engine.GateVerdict
	obs := changeBoundGateObserver("chg-9", func(_ context.Context, v engine.GateVerdict) { called = v })
	require.NotNil(t, obs)
	obs(context.Background(), engine.GateVerdict{ExecRunID: "run-ephemeral", GateName: "g"})
	assert.Equal(t, "chg-9", called.ChangeID, "the binding happens before the sink is called")
	assert.Equal(t, "run-ephemeral", called.ExecRunID, "and nothing the engine set is overwritten")
}
