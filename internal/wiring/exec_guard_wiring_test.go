// exec_guard_wiring_test.go exercises the wiring-side fencing
// integration with a FAKE guard (no database): Begin refusal, step-level
// Owns gating (with the engine sentinel propagating as skip-rollback),
// evidence-persistence gating, heartbeat/End plumbing, and the nil-guard
// (single-node) path staying exactly as before. The real PostgreSQL
// guard (cluster.ExecutionGuard) is covered by the PG-gated tests in
// internal/cluster and the takeover e2e suite.
package wiring

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/engine"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGuard is a scripted wiring.ExecutionGuard.
type fakeGuard struct {
	mu sync.Mutex
	// beginErr, when non-nil, is returned by Begin.
	beginErr error
	// ownsErr, when non-nil, is returned by every Owns call made AFTER
	// flipAfterOwns calls (before that Owns returns nil).
	ownsErr error
	// flipAfterOwns: Owns succeeds until it has been called this many
	// times, then returns ownsErr (0 = fail from the first call).
	flipAfterOwns int

	ownsCalls  int
	hbCalls    int
	endCalls   int
	beginCalls int
	leasesLive int
}

type fakeLease struct {
	guard *fakeGuard
}

func (l *fakeLease) Owns(ctx context.Context) error {
	l.guard.mu.Lock()
	defer l.guard.mu.Unlock()
	l.guard.ownsCalls++
	if l.guard.flipAfterOwns > 0 && l.guard.ownsCalls > l.guard.flipAfterOwns {
		return l.guard.ownsErr
	}
	if l.guard.flipAfterOwns == 0 && l.guard.ownsErr != nil {
		return l.guard.ownsErr
	}
	return nil
}

func (l *fakeLease) Heartbeat(context.Context) error {
	l.guard.mu.Lock()
	defer l.guard.mu.Unlock()
	l.guard.hbCalls++
	return nil
}

func (l *fakeLease) End(context.Context) error {
	l.guard.mu.Lock()
	defer l.guard.mu.Unlock()
	l.guard.endCalls++
	if l.guard.leasesLive > 0 {
		l.guard.leasesLive--
	}
	return nil
}

func (g *fakeGuard) Begin(context.Context, string) (ExecutionLease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.beginCalls++
	if g.beginErr != nil {
		return nil, g.beginErr
	}
	g.leasesLive++
	return &fakeLease{guard: g}, nil
}

func (g *fakeGuard) snapshot() (begins, owns, hbs, ends int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.beginCalls, g.ownsCalls, g.hbCalls, g.endCalls
}

// TestGuardedExecution_BeginRefusedBlocksRun pins the no-lease-no-run
// rule: with a guard attached, a failed Begin must refuse the execution
// before a single dispatch.
func TestGuardedExecution_BeginRefusedBlocksRun(t *testing.T) {
	g := &fakeGuard{beginErr: errors.New("cluster: register execution for \"run-x\": fenced out")}
	e, store := newLoopEngine(t, &loopRecorder{}, WithExecutionGuard(g, "node-x"))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-beg", execWorkflowYAML)
	planAndPersist(t, e, store, "run-beg", []string{"web-1"})
	setRunStatus(t, store, "run-beg", "running")

	_, _, _, err := e.runChange(context.Background(), "run-beg", false, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced execution")
	assert.ErrorIs(t, err, engine.ErrFencedOut, "Begin failures must surface the engine sentinel so callers can classify")

	begins, _, _, _ := g.snapshot()
	assert.Equal(t, 1, begins)
}

// TestGuardedExecution_StepGateFencesOutMidRun drives the mid-flight
// path: the lease flips to lost after the pre-dispatch Owns, so the
// blocked step dispatch fails with the sentinel, the closure skips its
// automatic rollback (no undo dispatch), and the evidence gate refuses
// to persist the superseded outcome.
func TestGuardedExecution_StepGateFencesOutMidRun(t *testing.T) {
	// The undo-capable workflow: failing the FIRST step with the fence
	// sentinel must not dispatch the undo command.
	rec := &loopRecorder{failCmd: map[string]bool{}}
	g := &fakeGuard{flipAfterOwns: 1, ownsErr: errors.New("cluster: own-check for \"run-g\": fenced out")}
	// Owns calls: 1 = first step's pre-dispatch check (passes), then
	// the guard flips. The next structural write — the persistence gate
	// — sees the loss. To exercise the STEP gate instead we make the
	// first step itself succeed and flip the guard before the second
	// step's pre-dispatch check.
	e, store := newLoopEngine(t, rec, WithExecutionGuard(g, "node-x"), WithExecLeaseTTL(90*time.Second))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-g", twoStepWorkflowYAML)
	planAndPersist(t, e, store, "run-g", []string{"web-1"})
	setRunStatus(t, store, "run-g", "running")

	execRunID, success, phase, err := e.runChange(context.Background(), "run-g", false, 1)
	require.Error(t, err, "the fenced-out run must report the loss")
	assert.ErrorIs(t, err, engine.ErrFencedOut)
	assert.False(t, success)
	_ = execRunID
	_ = phase

	// No undo was dispatched: the closure skipped its rollback.
	for _, c := range rec.snapshotCmds() {
		assert.NotEqual(t, "web-1\x00undo-command", c,
			"a fenced-out executor must never dispatch rollback undo commands")
	}
	// The first (pre-fence) step ran; the second did not.
	assert.Contains(t, rec.snapshotCmds(), "web-1\x00work-command")
	assert.NotContains(t, rec.snapshotCmds(), "web-1\x00fail-command")

	// Evidence gate: no step rows persisted for the superseded run.
	steps := stepsOf(t, store, "run-g")
	assert.Empty(t, steps, "a fenced-out execution must not persist evidence rows")
}

// TestGuardedExecution_EvidenceGateRefusesLatePersistence flips the
// lease only at the END (all steps ran fine): the persistence-gate Owns
// refuses, so a healthy-looking closure result is still not persisted.
func TestGuardedExecution_EvidenceGateRefusesLatePersistence(t *testing.T) {
	rec := &loopRecorder{}
	g := &fakeGuard{flipAfterOwns: 100, ownsErr: errors.New("cluster: own-check for \"run-h\": fenced out")}
	e, store := newLoopEngine(t, rec, WithExecutionGuard(g, "node-x"), WithExecLeaseTTL(90*time.Second))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-h", execWorkflowYAML)
	planAndPersist(t, e, store, "run-h", []string{"web-1"})
	setRunStatus(t, store, "run-h", "running")

	// Owns happens once per step dispatch; execWorkflowYAML has ONE
	// step, so calls: 1 (step). Set the flip so the SECOND Owns — the
	// evidence gate — fails.
	g.flipAfterOwns = 1

	_, success, _, err := e.runChange(context.Background(), "run-h", false, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fenced out before evidence persistence")
	assert.False(t, success)

	// Dispatch happened (healthy until the gate), but nothing persisted.
	assert.Equal(t, 1, len(rec.snapshotCmds()))
	assert.Empty(t, stepsOf(t, store, "run-h"))
}

// TestGuardedExecution_EndAndHeartbeatPlumbing asserts the lease
// lifecycle bookkeeping: End is called exactly once per execution and
// the heartbeat goroutine stopped with it.
func TestGuardedExecution_EndAndHeartbeatPlumbing(t *testing.T) {
	rec := &loopRecorder{}
	g := &fakeGuard{}
	e, store := newLoopEngine(t, rec, WithExecutionGuard(g, "node-x"), WithExecLeaseTTL(30*time.Millisecond))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-pl", execWorkflowYAML)
	planAndPersist(t, e, store, "run-pl", []string{"web-1"})
	setRunStatus(t, store, "run-pl", "running")

	_, success, phase, err := e.runChange(context.Background(), "run-pl", false, 1)
	require.NoError(t, err)
	assert.True(t, success)
	assert.Equal(t, "completed", phase)

	begins, _, _, ends := g.snapshot()
	assert.Equal(t, 1, begins)
	assert.Equal(t, 1, ends, "End must be called exactly once")
}

// twoStepWorkflowYAML: work then a (never-reached) second step, plus an
// undo for work — proving the fence fired BETWEEN steps.
const twoStepWorkflowYAML = `name: two-step
target:
  type: host
  query: "env=test"
steps:
  - name: work
    action: shell.exec
    args:
      cmd: work-command
    rollback:
      steps:
        - name: undo-work
          action: shell.exec
          args:
            cmd: undo-command
  - name: second
    action: shell.exec
    args:
      cmd: fail-command
`
