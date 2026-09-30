// exec_run_test.go is the execution acceptance suite for the wiring layer:
// a loopback channel transport stands in for ssh/winrm, so the full
// closure (plan artifact → lock → batch controller → executor modules →
// persist) runs for real in-process. It proves: real execution to
// completed with batch/step evidence; the stored-plan gate; failure →
// automatic rollback with undo evidence; the parallel-run cap; retry on
// an explicit host subset (batch row reuse); and manual rollback.

package wiring

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- loopback transport ------------------------------------------------------

// loopRecorder collects what the fake transport saw and can inject failures
// and blocking into Exec.
type loopRecorder struct {
	mu      sync.Mutex
	dials   []string
	cmds    []string // "host\x00cmd"
	closes  int
	failCmd map[string]bool // exact command → force exit 1
	release chan struct{}   // non-nil → Exec blocks until closed
	entered chan string     // non-nil → Exec signals the host once blocked
}

func (r *loopRecorder) recordDial(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dials = append(r.dials, host)
}

func (r *loopRecorder) recordCmd(host, cmd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, host+"\x00"+cmd)
}

func (r *loopRecorder) recordClose() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closes++
}

func (r *loopRecorder) snapshotCmds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cmds...)
}

func (r *loopRecorder) snapshotDials() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dials...)
}

func (r *loopRecorder) closeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closes
}

type loopChannel struct {
	host string
	rec  *loopRecorder

	mu        sync.Mutex
	connected bool
}

func (c *loopChannel) Connect(context.Context) error {
	c.rec.recordDial(c.host)
	c.mu.Lock()
	c.connected = true
	c.mu.Unlock()
	return nil
}

func (c *loopChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	if c.rec.release != nil {
		if c.rec.entered != nil {
			select {
			case c.rec.entered <- c.host:
			default:
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.rec.release:
		}
	}
	c.rec.recordCmd(c.host, cmd)
	if c.rec.failCmd[cmd] {
		return &channel.ExecResult{ExitCode: 1, Stderr: "loopback: forced failure for " + cmd}, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "loopback ok"}, nil
}

func (c *loopChannel) Upload(context.Context, string, io.Reader) error {
	return fmt.Errorf("loopback: Upload not supported")
}

func (c *loopChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, fmt.Errorf("loopback: Download not supported")
}

func (c *loopChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		c.connected = false
		c.rec.recordClose()
	}
	return nil
}

func (c *loopChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

type loopFactory struct{ rec *loopRecorder }

func (f loopFactory) Create(target channel.Target) (channel.Channel, error) {
	return &loopChannel{host: target.Host(), rec: f.rec}, nil
}

// newLoopEngine returns an Engine whose channel registry only serves the
// fake "local" transport, with the given hosts registered as active
// inventory targets.
func newLoopEngine(t *testing.T, rec *loopRecorder, opts ...Option) (*Engine, state.Store) {
	t.Helper()
	reg := channel.NewChannelRegistry()
	reg.Register("local", loopFactory{rec: rec})
	opts = append([]Option{WithChannelRegistry(reg)}, opts...)
	_, store := newTestEngine(t)
	return NewEngine(store, opts...), store
}

func seedLocalTargets(t *testing.T, store state.Store, hosts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, h := range hosts {
		require.NoError(t, store.UpsertTarget(ctx, &state.Target{
			ID:          "tgt-" + h,
			Hostname:    h,
			Port:        22,
			ChannelType: "local",
			Status:      "active",
			CreatedAt:   time.Now().UTC(),
		}))
	}
}

// planAndPersist generates the plan for a seeded run and writes the
// artifact, mirroring what ChangeService.PlanChange does.
func planAndPersist(t *testing.T, e *Engine, store state.Store, runID string, hosts []string) {
	t.Helper()
	ctx := context.Background()
	_, stored, err := e.GeneratePlan(ctx, runID, hosts)
	require.NoError(t, err)
	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	run.PlanJSON = stored.JSON
	run.PlanHash = stored.Hash
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(ctx, run))
}

func setRunStatus(t *testing.T, store state.Store, runID, status string) {
	t.Helper()
	ctx := context.Background()
	run, err := store.GetRun(ctx, runID)
	require.NoError(t, err)
	run.Status = status
	run.UpdatedAt = time.Now().UTC()
	require.NoError(t, store.UpdateRun(ctx, run))
}

func stepsOf(t *testing.T, store state.Store, runID string) []*state.Step {
	t.Helper()
	steps, err := store.ListSteps(context.Background(), state.StepFilter{RunID: runID, Limit: 200})
	require.NoError(t, err)
	return steps
}

func stepByNameHost(steps []*state.Step, name, host string) *state.Step {
	for _, s := range steps {
		if s.StepName == name && s.Host == host {
			return s
		}
	}
	return nil
}

// --- workflows ----------------------------------------------------------------

const execWorkflowYAML = `name: exec-test
target:
  type: host
  query: "env=test"
steps:
  - name: noop
    action: shell.exec
    args:
      cmd: noop-command
`

const rbWorkflowYAML = `name: rb-test
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
  - name: boom
    action: shell.exec
    args:
      cmd: fail-command
`

// --- tests --------------------------------------------------------------------

func TestRunChange_CompletesWithEvidence(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1", "web-2")
	seedRun(t, store, "run-exec", execWorkflowYAML)
	planAndPersist(t, e, store, "run-exec", []string{"web-1", "web-2"})
	setRunStatus(t, store, "run-exec", "running")

	execRunID, success, phase, err := e.runChange(context.Background(), "run-exec", false, 1)
	require.NoError(t, err)
	assert.True(t, success)
	assert.Equal(t, "completed", phase)
	assert.NotEmpty(t, execRunID)

	batches, err := store.ListBatches(context.Background(), state.BatchFilter{RunID: "run-exec"})
	require.NoError(t, err)
	require.Len(t, batches, 1)
	assert.Equal(t, 1, batches[0].BatchNo)
	assert.Equal(t, "completed", batches[0].Status)
	assert.Equal(t, 2, batches[0].TotalHosts)
	assert.Equal(t, 2, batches[0].Succeeded)

	steps := stepsOf(t, store, "run-exec")
	require.Len(t, steps, 2)
	for _, want := range []string{"web-1", "web-2"} {
		s := stepByNameHost(steps, "noop", want)
		require.NotNil(t, s, "missing step row for %s", want)
		assert.Equal(t, "success", s.Status)
		assert.Equal(t, "shell.exec", s.Action)
		require.NotNil(t, s.ExitCode)
		assert.Equal(t, 0, *s.ExitCode)
		assert.Equal(t, "loopback ok", s.Stdout)
	}

	// Channels are run-scoped: every dial was closed at teardown.
	assert.Equal(t, rec.closeCount(), len(rec.snapshotDials()), "every dialled channel must be closed")
}

func TestRunChange_RefusesMissingPlan(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-noplan", execWorkflowYAML)
	setRunStatus(t, store, "run-noplan", "running")

	_, _, _, err := e.runChange(context.Background(), "run-noplan", false, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no persisted plan")
	assert.Empty(t, rec.snapshotCmds(), "nothing may be dispatched without a plan")
}

func TestRunChange_FailureRollsBackAndPersistsUndo(t *testing.T) {
	rec := &loopRecorder{failCmd: map[string]bool{"fail-command": true}}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-rb", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-rb", []string{"web-1"})
	setRunStatus(t, store, "run-rb", "running")

	_, success, phase, err := e.runChange(context.Background(), "run-rb", false, 1)
	// The closure contract (per EngineAdapter.Run): err is reserved for
	// fatal engine errors; the outcome lives in success/phase, and the
	// failure cause lives in the persisted step rows.
	require.NoError(t, err)
	assert.False(t, success)
	// D-2 v2 verdict: the failed "boom" step has NO declared
	// compensation, so its (undetermined) side effects were never
	// undone — the run must NOT claim a clean "rolled_back". "work"
	// WAS compensated (evidence below), so this is a partial rollback,
	// not an incomplete one.
	assert.Equal(t, "rolled_back_partial", phase,
		"forced failure must trigger automatic rollback; missing compensation for the failed step must not read as a clean rollback")

	steps := stepsOf(t, store, "run-rb")
	fwd := stepByNameHost(steps, "work", "web-1")
	require.NotNil(t, fwd)
	assert.Equal(t, "success", fwd.Status)
	fwd2 := stepByNameHost(steps, "boom", "web-1")
	require.NotNil(t, fwd2)
	assert.Equal(t, "failed", fwd2.Status)
	assert.Contains(t, fwd2.Stderr, "forced failure")
	assert.NotZero(t, *fwd2.ExitCode)
	undo := stepByNameHost(steps, "undo-work", "web-1")
	require.NotNil(t, undo, "rollback evidence must be persisted")
	assert.Equal(t, "success", undo.Status)
	assert.Equal(t, "shell.exec", undo.Action)

	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00work-command")
	assert.Contains(t, cmds, "web-1\x00fail-command")
	assert.Contains(t, cmds, "web-1\x00undo-command")
}

func TestRunChange_ParallelRunCapFastFails(t *testing.T) {
	rec := &loopRecorder{
		release: make(chan struct{}),
		entered: make(chan string, 8),
	}
	e, store := newLoopEngine(t, rec, WithMaxParallelRuns(1))
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-slow", execWorkflowYAML)
	seedRun(t, store, "run-blocked", execWorkflowYAML)
	planAndPersist(t, e, store, "run-slow", []string{"web-1"})
	planAndPersist(t, e, store, "run-blocked", []string{"web-1"})

	done := make(chan error, 1)
	go func() {
		_, _, _, err := e.runChange(context.Background(), "run-slow", false, 1)
		done <- err
	}()

	// Wait until the first run is inside Exec (holding its slot).
	select {
	case <-rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run never reached Exec")
	}

	_, _, _, err := e.runChange(context.Background(), "run-blocked", false, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already executing")

	close(rec.release)
	require.NoError(t, <-done, "first run must complete once released")
}

func TestRetryChange_HostSubsetReusesBatchRows(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1", "web-2")
	seedRun(t, store, "run-retry", execWorkflowYAML)
	planAndPersist(t, e, store, "run-retry", []string{"web-1", "web-2"})

	// First full execution, then simulate a failure verdict on the run so
	// retry's precondition (failed/rolled_back) holds.
	_, _, _, err := e.runChange(context.Background(), "run-retry", false, 2)
	require.NoError(t, err)
	setRunStatus(t, store, "run-retry", "failed")
	before := stepsOf(t, store, "run-retry")
	require.Len(t, before, 2)
	cmdsBefore := len(rec.snapshotCmds())

	require.NoError(t, e.retryChange(context.Background(), "run-retry", false, []string{"web-2"}))

	// Only web-2 was re-dispatched by the retry (commands recorded after
	// the retry started).
	for _, c := range rec.snapshotCmds()[cmdsBefore:] {
		assert.NotEqual(t, "web-1\x00noop-command", c, "web-1 must not be re-executed")
	}

	run, err := store.GetRun(context.Background(), "run-retry")
	require.NoError(t, err)
	assert.Equal(t, "completed", run.Status, "successful retry must write the terminal status")

	// Batch rows are reused (UNIQUE(run_id, batch_no)); step rows append.
	batches, err := store.ListBatches(context.Background(), state.BatchFilter{RunID: "run-retry"})
	require.NoError(t, err)
	assert.Len(t, batches, 1)
	steps := stepsOf(t, store, "run-retry")
	assert.Len(t, steps, 3)

	traces, err := store.ListTraces(context.Background(), state.TraceFilter{RunID: "run-retry"})
	require.NoError(t, err)
	found := false
	for _, tr := range traces {
		if tr.Event == "retry_finished" {
			found = true
		}
	}
	assert.True(t, found, "retry_finished trace must be recorded")
}

func TestRetryChange_WithoutHostsOrReplanRefused(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-retry2", execWorkflowYAML)
	planAndPersist(t, e, store, "run-retry2", []string{"web-1"})
	setRunStatus(t, store, "run-retry2", "failed")

	err := e.retryChange(context.Background(), "run-retry2", false, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "explicit host subset")
}

// TestRetryChange_TerminalSupersededConcurrently pins the B1 discipline
// on the wiring side: retryChange's terminal write is a CAS from
// "running", so when another actor (a failover takeover) moves the run
// while the retry executes, the retry's verdict must not overwrite the
// winner's state. The race is staged deterministically: the loopback
// channel blocks inside Exec, the takeover transition happens from the
// test goroutine, then the channel is released so the retry finishes and
// attempts its (now stale) terminal write.
func TestRetryChange_TerminalSupersededConcurrently(t *testing.T) {
	rec := &loopRecorder{
		release: make(chan struct{}),
		entered: make(chan string, 8),
	}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-retry3", execWorkflowYAML)
	planAndPersist(t, e, store, "run-retry3", []string{"web-1"})
	setRunStatus(t, store, "run-retry3", "failed")

	done := make(chan error, 1)
	go func() {
		done <- e.retryChange(context.Background(), "run-retry3", false, []string{"web-1"})
	}()

	// Wait until the retry is inside Exec (past its running CAS), then
	// simulate the takeover winning the run.
	select {
	case <-rec.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("retry never reached Exec")
	}
	run, err := store.GetRun(context.Background(), "run-retry3")
	require.NoError(t, err)
	require.Equal(t, "running", run.Status)
	ok, err := store.UpdateRunStatusIf(context.Background(), "run-retry3", "running", "interrupted", utcNowStub())
	require.NoError(t, err)
	require.True(t, ok, "takeover CAS must win while the retry is blocked in Exec")

	close(rec.release)
	err = <-done
	require.Error(t, err, "the superseded retry must report the loss, not claim success")
	assert.Contains(t, err.Error(), "superseded")

	run, err = store.GetRun(context.Background(), "run-retry3")
	require.NoError(t, err)
	assert.Equal(t, "interrupted", run.Status,
		"the takeover's terminal state must survive the retry executor's late verdict")
}

// utcNowStub mirrors wiring.utcNow for tests in this file that need the
// same timestamp shape without importing the unexported helper.
func utcNowStub() time.Time { return time.Now().UTC() }

func TestRollbackChange_ManualUndo(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-manual-rb", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-manual-rb", []string{"web-1"})

	// D-2 v2 design item 1: the manual rollback compensates exactly what
	// the steps-table evidence says ran. This run was NEVER applied — no
	// forward rows exist — so nothing may be dispatched: undoing state
	// that was never changed is the P0-3 bug this ledger exists to stop.
	rbID, hosts, err := e.rollbackChange(context.Background(), "run-manual-rb", "", true)
	require.NoError(t, err, "a no-evidence rollback is a clean no-op, not an error")
	assert.Contains(t, rbID, "rb-")
	assert.Empty(t, hosts, "never-applied run: nothing to compensate")

	cmds := rec.snapshotCmds()
	assert.NotContains(t, cmds, "web-1\x00undo-command",
		"without forward evidence no undo may be dispatched")
	assert.NotContains(t, cmds, "web-1\x00work-command")

	steps := stepsOf(t, store, "run-manual-rb")
	undo := stepByNameHost(steps, "undo-work", "web-1")
	assert.Nil(t, undo, "no undo evidence may be written when nothing was undone")
}

const snapWorkflowYAML = `name: snap-gate
target:
  type: host
  query: "env=test"
steps:
  - name: snap-step
    action: shell.exec
    args:
      cmd: snap-command
    rollback:
      strategy: snapshot
`

// TestRunChange_SnapshotPlanWithoutStoreFailsClosed pins D-2 v2 design
// item 5: a plan declaring snapshot-based rollback must be refused BEFORE
// any dispatch when no snapshot store is configured — the gap used to
// surface only mid-rollback as a "restore not wired" skip, after the
// forward state had already changed.
func TestRunChange_SnapshotPlanWithoutStoreFailsClosed(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec) // no WithSnapshotDir on purpose
	seedLocalTargets(t, store, "web-1")
	seedRun(t, store, "run-snap-gate", snapWorkflowYAML)
	planAndPersist(t, e, store, "run-snap-gate", []string{"web-1"})
	setRunStatus(t, store, "run-snap-gate", "running")

	_, _, _, err := e.runChange(context.Background(), "run-snap-gate", false, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares snapshot rollback")
	assert.Contains(t, err.Error(), "fail-closed")
	assert.Empty(t, rec.snapshotCmds(), "nothing may be dispatched for a refused snapshot plan")
}

// TestPlanNeedsSnapshot unit-covers the gate predicate across rollback
// strategies: only strategy-"snapshot" declarations demand a snapshot store.
func TestPlanNeedsSnapshot(t *testing.T) {
	snap := newD2WiringPlan("snapshot")
	undo := newD2WiringPlan("undo-action")
	none := newD2WiringPlan("")

	assert.True(t, planNeedsSnapshot(snap))
	assert.False(t, planNeedsSnapshot(undo))
	assert.False(t, planNeedsSnapshot(none))
}

// newD2WiringPlan builds a one-batch plan whose step declares a rollback
// with the given strategy ("" = no rollback spec at all).
func newD2WiringPlan(strategy string) *plan.Plan {
	ps := plan.PlanStep{Name: "s1", Module: "m", Action: "a"}
	if strategy != "" {
		ps.Rollback = &dsl.RollbackSpec{Strategy: strategy}
	}
	return &plan.Plan{
		ID:      "plan-snap-unit",
		Batches: []plan.Batch{{Index: 0, Targets: []string{"h1"}, Steps: []plan.PlanStep{ps}}},
	}
}

// TestRollbackChange_ManualLedgerFromPartialEvidence is the D-2 v2 design
// test («用手工构造部分执行覆盖»): two hosts in the plan, forward evidence
// for web-1 ONLY — the manual rollback must compensate web-1 alone and
// never touch web-2, whose state was never changed.
func TestRollbackChange_ManualLedgerFromPartialEvidence(t *testing.T) {
	rec := &loopRecorder{}
	e, store := newLoopEngine(t, rec)
	seedLocalTargets(t, store, "web-1", "web-2")
	seedRun(t, store, "run-partial-ev", rbWorkflowYAML)
	planAndPersist(t, e, store, "run-partial-ev", []string{"web-1", "web-2"})

	// 手工构造部分执行: a batch row plus forward step evidence for web-1 only.
	ctx := context.Background()
	now := utcNowStub()
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:          "bat-partial-ev",
		RunID:       "run-partial-ev",
		BatchNo:     1,
		Status:      "failed",
		TotalHosts:  2,
		Succeeded:   1,
		Failed:      0,
		StartedAt:   &now,
		CompletedAt: &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:       "stp-ev-1",
		RunID:    "run-partial-ev",
		BatchID:  "bat-partial-ev",
		Host:     "web-1",
		StepName: "work",
		Action:   "shell.exec",
		Status:   "success",
	}))

	rbID, hosts, err := e.rollbackChange(context.Background(), "run-partial-ev", "", true)
	require.NoError(t, err, "the evidenced compensation completes cleanly")
	assert.Contains(t, rbID, "rb-")
	assert.Equal(t, []string{"web-1"}, hosts, "only the evidenced host may be reported rolled back")

	cmds := rec.snapshotCmds()
	assert.Contains(t, cmds, "web-1\x00undo-command", "web-1's evidenced step must be compensated")
	assert.NotContains(t, cmds, "web-2\x00undo-command",
		"web-2 has no forward evidence: compensating it would touch unchanged state")

	// Undo evidence lands for web-1 only.
	steps := stepsOf(t, store, "run-partial-ev")
	undo := stepByNameHost(steps, "undo-work", "web-1")
	require.NotNil(t, undo, "rollback evidence must be persisted")
	assert.Equal(t, "success", undo.Status)
	assert.Nil(t, stepByNameHost(steps, "undo-work", "web-2"))
}
