// run.go implements the execution closures of the EngineAdapter seam:
// Run drives the stored plan artifact through the ClosureRunner
// synchronously; Retry re-executes an explicit host subset (or a fresh
// plan when replan is requested); Rollback reverses the stored plan via
// rollback.Manager. All three honour the process-wide parallel-run cap and
// re-verify the plan artifact before touching any target.
//
// Snapshot wiring (design §4.4.4.2 / §4.4.6.3): when a snapshot store is
// configured (--engine-snapshot-dir / WithSnapshotDir), every execution
// installs a channel-aware remoteSnapshotter on the closure (capture
// before the first batch) and the matching SnapshotRestoreFunc on the
// rollback manager (restore instead of undo steps for strategy
// "snapshot"). Without a store both halves are disabled: capture is
// skipped (pre-wiring behaviour) and snapshot steps restore as "not
// wired" skips — never silent undo actions. Snapshots are keyed by the
// CHANGE id so the manual RollbackChange path (which knows no closure
// run id) finds them too.

package wiring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/lock"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/state"
	"github.com/nexus/levee/internal/verify"
)

// acquire takes one of the parallel-run slots (non-blocking). The returned
// release func must be called when the run finishes.
func (e *Engine) acquire(changeID string) (func(), error) {
	select {
	case e.sem <- struct{}{}:
		return func() { <-e.sem }, nil
	default:
		return nil, fmt.Errorf("wiring: refusing to start change %q: %d changes are already executing (cap: --engine-max-parallel-runs=%d)",
			changeID, len(e.sem), e.maxParallelRuns)
	}
}

// newRunRunner builds a FRESH set of per-run subsystems. ClosureRunner is
// single-flight and its subsystems carry per-run state (locks keyed by run,
// materialised gates), so nothing here may be shared across runs.
//
// Policy choices (docs/design-engine-wiring.md §A2):
//   - batch/target error policies are abort: the first failure stops the
//     run and triggers rollback of executed batches;
//   - rollback whitelists every module: a change that can be planned can
//     be undone by the same machinery;
//   - declared workflow gates are materialised from the plan and enforced;
//     gates the engine cannot execute (unknown type, missing runtime for
//     slo/human checks) fail the run rather than silently passing;
//   - the host guard re-validates frozen targets between lock acquisition
//     and mutation (planning-time check alone is stale).
func (e *Engine) newRunRunner(rx *runExec, changeID string) *engine.ClosureRunner {
	lockMgr := lock.NewLockManager(lock.NewLockStore(e.store), e.store)
	lockMgr.SetTTL(e.lockTTL)

	gateMgr := verify.NewGateManager()

	// Snapshot halves: the capture hook (engine.WithSnapshotter) and the
	// restore callback (rollback.WithSnapshotRestore) share one manager
	// over one store. Both stay nil when no snapshot dir is configured —
	// the closure then skips capture entirely (its nil check) and the
	// restore side records "not wired" skips.
	var snapHook engine.Snapshotter
	snapOpts := make([]rollback.ManagerOption, 0, 5)
	if rx != nil && e.snapshotDir != "" {
		if store, err := rollback.NewFileSnapshotStore(e.snapshotDir); err != nil {
			log.Error("snapshot store init failed; snapshot capture disabled", "error", err)
		} else if mgr, err := rollback.NewSnapshotManager(store); err != nil {
			log.Error("snapshot manager init failed; snapshot capture disabled", "error", err)
		} else {
			snapshotter := newRemoteSnapshotter(rx, mgr, changeID)
			snapHook = snapshotter
			snapOpts = append(snapOpts,
				rollback.WithSnapshotRestore(snapshotter.RestoreForStep),
				rollback.WithRunID(changeID))
		}
	}

	rollbackMgr := rollback.NewManager(append([]rollback.ManagerOption{
		rollback.WithWhitelistAll(),
		rollback.WithConcurrency(e.rollbackConcurrency),
		rollback.WithStopOnError(false),
	}, snapOpts...)...)

	batchCtrl := batch.NewController(
		batch.WithBatchErrorPolicy(batch.PolicyAbort),
		batch.WithTargetErrorPolicy(batch.PolicyAbort),
	)

	store := e.store
	cr := engine.NewClosureRunner(store, lockMgr, gateMgr, rollbackMgr, batchCtrl, nil,
		engine.WithHostGuard(func(ctx context.Context, hosts []string) error {
			return inventory.ValidateNotFrozen(ctx, store, hosts)
		}),
		// Gate runtime: only the slo gate consumes PrometheusURL. With the
		// default (empty) the materialisation of an slo/human gate still
		// fails closed exactly as it would with no runtime attached.
		engine.WithGateRuntime(engine.GateRuntime{PrometheusURL: e.gatePrometheusURL}),
	)
	if snapHook != nil {
		// The WithSnapshotter option is applied post-construction via a
		// dedicated setter to keep the constructor signature stable.
		cr.SetSnapshotter(snapHook)
	}
	return cr
}

// runChange is the EngineAdapter.Run closure. ChangeService has already
// verified the artifact gates and transitioned the run to "running" before
// invoking us; the artifact is re-verified here defensively.
func (e *Engine) runChange(ctx context.Context, changeID string, _ bool, maxConcurrency int32) (string, bool, string, error) {
	release, err := e.acquire(changeID)
	if err != nil {
		return "", false, "", err
	}
	defer release()

	p, err := e.loadStoredPlan(ctx, changeID)
	if err != nil {
		return "", false, "", err
	}
	if maxConcurrency > 0 {
		for i := range p.Batches {
			p.Batches[i].MaxConcurrency = int(maxConcurrency)
		}
	}
	return e.executePlan(ctx, changeID, p, false)
}

// executePlan drives the full closure once and persists batch/step rows
// under the change's run id (operator-visible evidence). Callers own the
// parallel-run slot (see acquire); executePlan itself does not take one.
//
// When an execution guard is attached (cluster mode), the whole closure
// runs fenced: the lease is claimed before dispatch, renewed by a
// background heartbeat (TTL/3, independent of step progress — slow steps
// never let the lease lapse), re-validated before evidence persistence,
// and released at the end. Losing the lease mid-flight surfaces as an
// engine.ErrFencedOut error chain: step dispatch stops immediately, the
// closure skips its automatic rollback (see closure.go), and this
// function refuses to persist the superseded evidence.
//
// resumable selects retry-from-interrupt semantics: when true, batches whose
// every (step, host) combination already has a "success" row from an
// idempotent module are skipped (their evidence is recorded as "skipped"),
// so a node that crashed mid-flight resumes from where it broke instead of
// re-running the whole plan. resumable MUST be true only when retrying a run
// that ended in the "interrupted" terminal state — for fresh applies and for
// failed/rolled_back retries it must be false (a rollback may have undone a
// previously-successful step, making the skip unsafe).
func (e *Engine) executePlan(ctx context.Context, changeID string, p *plan.Plan, resumable bool) (execRunID string, success bool, phase string, err error) {
	// Fencing begin: in cluster mode an execution without a lease would
	// be an invisible takeover candidate — refuse outright instead.
	var lease ExecutionLease
	if e.guard != nil {
		lease, err = e.guard.Begin(ctx, changeID)
		if err != nil {
			return "", false, "", fmt.Errorf("wiring: fenced execution of %q: %w", changeID, err)
		}
		defer func() { _ = lease.End(context.WithoutCancel(ctx)) }()
		stopHeartbeat := e.startLeaseHeartbeat(lease)
		defer stopHeartbeat()
	}

	// Resumable retry: drop batches that already completed idempotently and
	// persist "skipped" evidence for audit completeness. The engine then runs
	// the filtered plan unchanged.
	if resumable {
		skip, serr := e.completedIdempotentBatches(ctx, changeID, p)
		if serr == nil && len(skip) > 0 {
			resumePlan, skipped := buildResumePlan(p, skip)
			e.persistResumeEvidence(ctx, changeID, skipped)
			p = resumePlan
		}
	}

	// D-2 v2 design item 5 (fail-closed): a plan that declares
	// snapshot-based rollback requires a wired snapshot store — without
	// one, capture never happens and the rollback would discover
	// mid-unwind that its restore basis does not exist. Refuse BEFORE
	// any dispatch instead of skipping afterwards.
	if err := e.checkSnapshotCapability(p); err != nil {
		return "", false, "", err
	}

	rx, err := newRunExec(ctx, e, lease)
	if err != nil {
		return "", false, "", err
	}
	defer rx.close()

	// Fresh subsystem instances per execution (see newRunRunner). The
	// runner needs rx (the snapshotter captures over its channel cache),
	// so it is assembled AFTER the runExec exists.
	runner := e.newRunRunner(rx, changeID)

	res, runErr := runner.Run(ctx, p, rx.executeFunc())
	if res == nil {
		// Only possible for an extraordinary rand failure in Run.
		return "", false, "", errors.Join(
			fmt.Errorf("wiring: closure run for change %q produced no result", changeID), runErr)
	}

	// Evidence gate: a superseded executor must not persist its late
	// evidence — the takeover's interrupted state and its audit chain
	// must not be polluted by rows from an execution the cluster has
	// already written off. Dirty evidence is worse than missing
	// evidence (the takeover trace records what happened).
	if lease != nil {
		if oerr := lease.Owns(context.WithoutCancel(ctx)); oerr != nil {
			return res.RunID, false, string(res.Phase), fmt.Errorf(
				"wiring: %q fenced out before evidence persistence: %w", changeID, oerr)
		}
	}

	persistErr := e.persistClosureResults(ctx, changeID, p, res, rx.snapshotOutputs())
	if runErr != nil {
		if persistErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("persisting step evidence: %w", persistErr))
		}
		return res.RunID, false, string(res.Phase), runErr
	}
	if persistErr != nil {
		// Execution outcome stands, but evidence is incomplete — report the
		// failure on top of the phase so the caller cannot mistake this for
		// a clean success.
		return res.RunID, false, string(res.Phase), fmt.Errorf("wiring: persisting step evidence: %w", persistErr)
	}
	return res.RunID, res.Phase == engine.PhaseCompleted, string(res.Phase), nil
}

// startLeaseHeartbeat renews the execution lease at TTL/3 from a
// background goroutine until the returned stop function is called.
// Renewal is deliberately decoupled from step progress: the TTL bounds
// post-crash detection latency, not execution duration, so a healthy
// executor running slow steps keeps its lease indefinitely. A failed
// renewal (fenced out) stops the loop — the step-dispatch Owns gate
// will surface the loss at the next structural write.
func (e *Engine) startLeaseHeartbeat(lease ExecutionLease) (stop func()) {
	interval := e.execLeaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := lease.Heartbeat(context.Background()); err != nil {
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// retryChange is the EngineAdapter.Retry closure. It re-executes synchronously
// and owns the run's status lifecycle (CAS into running, terminal write
// out) exactly like apply, so the RPC handler only reflects the outcome.
//
// Honest scope: with replan=false the host subset MUST be explicit (the
// RetryHost path passes it; deriving "failed hosts" from step rows cannot
// tell a still-failing host from one a previous retry already fixed).
// With replan=true a fresh plan is generated and persisted (for the request
// hosts or the stored plan's hosts) and re-executed whole — the operator
// re-approved by invoking retry with replan.
func (e *Engine) retryChange(ctx context.Context, changeID string, replan bool, targetHosts []string) error {
	release, err := e.acquire(changeID)
	if err != nil {
		return err
	}
	defer release()

	p, err := e.loadStoredPlan(ctx, changeID)
	if err != nil {
		return err
	}

	if replan {
		hosts := targetHosts
		if len(hosts) == 0 {
			hosts = globalTargets(p)
		}
		_, stored, err := e.GeneratePlan(ctx, changeID, hosts)
		if err != nil {
			return fmt.Errorf("wiring: replan: %w", err)
		}
		run, err := e.store.GetRun(ctx, changeID)
		if err != nil || run == nil {
			return fmt.Errorf("wiring: get run for replan: %w", err)
		}
		run.PlanJSON = stored.JSON
		run.PlanHash = stored.Hash
		run.UpdatedAt = utcNow()
		if err := e.store.UpdateRun(ctx, run); err != nil {
			return fmt.Errorf("wiring: persist replan: %w", err)
		}
		p, err = e.loadStoredPlan(ctx, changeID)
		if err != nil {
			return err
		}
	} else if len(targetHosts) == 0 {
		return fmt.Errorf("wiring: retry without replan requires an explicit host subset (retry with target_hosts, or replan=true)")
	} else {
		p = narrowPlan(p, targetHosts)
		if len(p.Batches) == 0 {
			return fmt.Errorf("wiring: retry hosts %v match no batch of the stored plan", targetHosts)
		}
	}

	// Status lifecycle: only failed/rolled_back runs reach here (RPC guard);
	// CAS into running so a concurrent retry/apply cannot double-execute.
	run, err := e.store.GetRun(ctx, changeID)
	if err != nil || run == nil {
		return fmt.Errorf("wiring: get run for retry: %w", err)
	}
	old := run.Status
	ok, err := e.store.UpdateRunStatusIf(ctx, changeID, old, "running", utcNow())
	if err != nil {
		return fmt.Errorf("wiring: retry transition: %w", err)
	}
	if !ok {
		return fmt.Errorf("wiring: retry refused: change %q status changed concurrently (no longer %q)", changeID, old)
	}

	// A retry out of "interrupted" resumes from where the crashed execution
	// stopped: batches whose every step already succeeded via an idempotent
	// module are skipped. Other sources (failed/rolled_back) always re-run
	// the full plan — a rollback may have undone a previously-successful
	// step, so skipping would be unsafe.
	execRunID, _, phase, err := e.executePlan(ctx, changeID, p, old == "interrupted")
	final := "failed"
	switch {
	case phase == string(engine.PhaseRolledBack),
		phase == string(engine.PhasePartialRollback),
		phase == string(engine.PhaseRollbackIncomplete):
		// D-2 v2 design item 4: each rollback verdict keeps its own run
		// status; a partial or incomplete rollback must not read as a
		// clean "rolled_back" (or as a plain failure).
		final = phase
	case err == nil && phase == string(engine.PhaseCompleted):
		final = "completed"
	}
	// Terminal write via CAS from "running": if another actor (failover
	// takeover, a concurrent apply) already moved the run, the retry
	// executor must not overwrite the winner's state. The run row read
	// below names the authoritative status either way.
	ok, casErr := e.store.UpdateRunStatusIf(ctx, changeID, "running", final, utcNow())
	if casErr != nil {
		err = errors.Join(err, fmt.Errorf("wiring: retry final status write: %w", casErr))
	} else if !ok {
		err = errors.Join(err, fmt.Errorf("wiring: retry verdict superseded: change %q no longer running (concurrently transitioned)", changeID))
	}
	_ = e.store.CreateTrace(ctx, &state.Trace{
		ID:        newID("trc-"),
		RunID:     changeID,
		Event:     "retry_finished",
		Actor:     "engine",
		Timestamp: utcNow(),
	})
	if err != nil {
		return err
	}
	_ = execRunID
	return nil
}

// rollbackChange is the EngineAdapter.Rollback closure: it reverses the
// stored plan with rollback.Manager (the same machinery the closure path
// uses on failure) and persists the undo evidence. The RPC handler owns
// the run's rolled_back status.
func (e *Engine) rollbackChange(ctx context.Context, changeID, _ string, _ bool) (string, []string, error) {
	release, err := e.acquire(changeID)
	if err != nil {
		return "", nil, err
	}
	defer release()

	p, err := e.loadStoredPlan(ctx, changeID)
	if err != nil {
		return "", nil, err
	}

	// Manual rollback is deliberately OUTSIDE the fencing perimeter
	// (design §7.5-Q3): it does not transit through "running" and does
	// not register an execution lease. A node dying mid-rollback leaves
	// the run in its source (non-running) terminal state, where the
	// takeover loop's running-only scan never picks it up and an
	// operator can simply re-issue the rollback.
	rx, err := newRunExec(ctx, e, nil)
	if err != nil {
		return "", nil, err
	}
	defer rx.close()

	mgrOpts := []rollback.ManagerOption{
		rollback.WithWhitelistAll(),
		rollback.WithConcurrency(e.rollbackConcurrency),
		rollback.WithStopOnError(false),
	}
	// Manual rollback shares the snapshot store: strategy-"snapshot"
	// steps restore their pre-apply capture (keyed by the CHANGE id, so
	// this path finds them without knowing any closure run id).
	if e.snapshotDir != "" {
		if store, err := rollback.NewFileSnapshotStore(e.snapshotDir); err != nil {
			log.Error("snapshot store init failed; manual rollback runs without snapshot restore", "error", err)
		} else if snapMgr, err := rollback.NewSnapshotManager(store); err != nil {
			log.Error("snapshot manager init failed; manual rollback runs without snapshot restore", "error", err)
		} else {
			snapshotter := newRemoteSnapshotter(rx, snapMgr, changeID)
			mgrOpts = append(mgrOpts,
				rollback.WithSnapshotRestore(snapshotter.RestoreForStep),
				rollback.WithRunID(changeID))
		}
	}
	mgr := rollback.NewManager(mgrOpts...)

	// D-2 v2 design item 1: the manual path holds no BatchResults, so
	// its execution ledger derives from the persisted forward step
	// evidence (the steps table) — compensate what provably ran, and
	// nothing else. An unreadable evidence set fails closed: falling
	// back to the whole plan would restore the pre-D-2 over-broad undo.
	ledger, lerr := e.ledgerFromStoredSteps(ctx, changeID, p)
	if lerr != nil {
		return "", nil, lerr
	}
	res := mgr.RollbackWithLedger(ctx, p, rx.executeFunc(), ledger)

	rbID := newID("rb-")
	persistErr := e.persistRollbackResults(ctx, changeID, res, rx.snapshotOutputs())
	hosts := rolledBackHosts(res)

	if !res.Success {
		cause := res.Error
		if cause == nil {
			cause = fmt.Errorf("partial rollback: some undo steps failed")
		}
		if persistErr != nil {
			cause = errors.Join(cause, fmt.Errorf("persisting rollback evidence: %w", persistErr))
		}
		return rbID, hosts, cause
	}
	if persistErr != nil {
		return rbID, hosts, fmt.Errorf("wiring: rollback succeeded but persisting evidence failed: %w", persistErr)
	}
	return rbID, hosts, nil
}

// planNeedsSnapshot reports whether any step of p declares the
// strategy-"snapshot" rollback basis (D-2 v2 design item 5).
func planNeedsSnapshot(p *plan.Plan) bool {
	for _, b := range p.Batches {
		for _, s := range b.Steps {
			if s.Rollback != nil && s.Rollback.Strategy == "snapshot" {
				return true
			}
		}
	}
	return false
}

// checkSnapshotCapability fails closed when p declares snapshot-based
// rollback but this engine cannot capture/restore (no
// --engine-snapshot-dir, or the store/manager cannot be constructed).
// Called before any dispatch so the gap surfaces as a refusal, never as
// a mid-rollback "restore not wired" skip (D-2 v2 design item 5).
func (e *Engine) checkSnapshotCapability(p *plan.Plan) error {
	if !planNeedsSnapshot(p) {
		return nil
	}
	if e.snapshotDir == "" {
		return fmt.Errorf("wiring: plan %q declares snapshot rollback but no snapshot store is configured (--engine-snapshot-dir); refusing to execute (fail-closed, D-2 v2)", p.ID)
	}
	snapStore, err := rollback.NewFileSnapshotStore(e.snapshotDir)
	if err != nil {
		return fmt.Errorf("wiring: plan %q declares snapshot rollback but the snapshot store at %q is unusable: %w (fail-closed, D-2 v2)", p.ID, e.snapshotDir, err)
	}
	if _, err := rollback.NewSnapshotManager(snapStore); err != nil {
		return fmt.Errorf("wiring: plan %q declares snapshot rollback but the snapshot manager is unusable: %w (fail-closed, D-2 v2)", p.ID, err)
	}
	return nil
}

// ledgerFromStoredSteps derives the manual-rollback execution ledger from
// persisted forward step evidence (D-2 v2 design item 1). Only rows that
// match a FORWARD plan step of p on its declaring batch/target count:
//   - status "success" → ran (MarkRan)
//   - status "failed"  → dispatched and failed (MarkUnknown: it still needs
//     its declared compensation; residue undetermined)
//   - status "skipped" → resume marker, not a dispatch — ignored (the
//     original success row exists alongside it)
//
// Undo rows written by a previous rollback do not match forward step names
// in the normal case and are ignored; a workflow that names an undo step
// identically to a forward step is the documented ambiguity left to the
// future compensation-idempotency extension point. With no forward evidence
// at all the ledger is empty and nothing is compensated — pre-engine runs
// cannot reach this path (loadStoredPlan refuses runs without a stored
// artifact).
func (e *Engine) ledgerFromStoredSteps(ctx context.Context, changeID string, p *plan.Plan) (*rollback.ExecutionLedger, error) {
	rows, err := e.store.ListSteps(ctx, state.StepFilter{RunID: changeID})
	if err != nil {
		return nil, fmt.Errorf("wiring: list steps for rollback ledger: %w", err)
	}
	forward := make(map[string]map[string]bool)
	for _, b := range p.Batches {
		for _, t := range b.Targets {
			if forward[t] == nil {
				forward[t] = make(map[string]bool)
			}
			for _, s := range b.Steps {
				forward[t][s.Name] = true
			}
		}
	}
	ledger := rollback.NewExecutionLedger()
	for _, row := range rows {
		if row == nil || !forward[row.Host][row.StepName] {
			continue
		}
		switch row.Status {
		case "success":
			ledger.MarkRan(row.Host, row.StepName)
		case "failed":
			ledger.MarkUnknown(row.Host, row.StepName)
		}
	}
	return ledger, nil
}

// --- stored plan helpers ----------------------------------------------------

// loadStoredPlan reads the persisted plan artifact and re-verifies its
// canonical hash. Mirrors the ChangeService gate defensively: the engine
// executes nothing it cannot prove is the approved plan.
func (e *Engine) loadStoredPlan(ctx context.Context, changeID string) (*plan.Plan, error) {
	run, err := e.store.GetRun(ctx, changeID)
	if err != nil {
		return nil, fmt.Errorf("wiring: get run: %w", err)
	}
	if run == nil {
		return nil, fmt.Errorf("wiring: change %q not found", changeID)
	}
	if run.PlanJSON == "" {
		return nil, fmt.Errorf("wiring: change %q has no persisted plan; plan (and re-approve) before executing", changeID)
	}
	var p plan.Plan
	if err := json.Unmarshal([]byte(run.PlanJSON), &p); err != nil {
		return nil, fmt.Errorf("wiring: stored plan for %q is corrupt: %w", changeID, err)
	}
	if !plan.VerifyHash(&p, run.PlanHash) {
		return nil, fmt.Errorf("wiring: stored plan for %q does not match its plan hash (drift or corruption); re-plan required", changeID)
	}
	return &p, nil
}

// narrowPlan returns a copy of p restricted to the given hosts; batches left
// without targets are dropped, batch indices are preserved (the batch row
// key is the original batch_no).
func narrowPlan(p *plan.Plan, hosts []string) *plan.Plan {
	want := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		want[h] = true
	}
	out := &plan.Plan{
		ID:            p.ID,
		WorkflowName:  p.WorkflowName,
		CreatedAt:     p.CreatedAt,
		RiskScore:     p.RiskScore,
		RiskFactors:   p.RiskFactors,
		ApprovalFloor: p.ApprovalFloor,
		Approval:      p.Approval,
		Rollback:      p.Rollback,
		Gate:          p.Gate,
	}
	for _, b := range p.Batches {
		var keep []string
		for _, t := range b.Targets {
			if want[t] {
				keep = append(keep, t)
			}
		}
		if len(keep) == 0 {
			continue
		}
		nb := plan.Batch{
			Index:          b.Index,
			Targets:        keep,
			Steps:          b.Steps,
			MaxConcurrency: b.MaxConcurrency,
			Gate:           b.Gate,
		}
		out.Batches = append(out.Batches, nb)
	}
	for _, b := range out.Batches {
		out.TotalTargets += len(b.Targets)
	}
	return out
}

// globalTargets returns the de-duplicated target set of the plan.
func globalTargets(p *plan.Plan) []string {
	seen := make(map[string]bool)
	var out []string
	for _, b := range p.Batches {
		for _, t := range b.Targets {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// rolledBackHosts lists the distinct hosts whose undo steps ran (skipped
// steps do not count).
func rolledBackHosts(res *rollback.RollbackResult) []string {
	if res == nil {
		return nil
	}
	seen := make(map[string]bool)
	var hosts []string
	for _, br := range res.BatchResults {
		for _, tr := range br.TargetResults {
			ran := false
			for _, sr := range tr.StepResults {
				if !sr.Skipped {
					ran = true
				}
			}
			if ran && !seen[tr.Target] {
				seen[tr.Target] = true
				hosts = append(hosts, tr.Target)
			}
		}
	}
	return hosts
}

// newID mints a prefixed random identifier (same shape as the gRPC layer's
// helpers, kept local so the engine does not import the service package).
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinary; fall back to a fixed
		// prefix collision risk is not worth a panic.
		return prefix + "0000000000000000"
	}
	return prefix + hex.EncodeToString(b[:])
}
