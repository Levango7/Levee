// run.go implements the execution closures of the EngineAdapter seam:
// Run drives the stored plan artifact through the ClosureRunner
// synchronously; Retry re-executes an explicit host subset (or a fresh
// plan when replan is requested); Rollback reverses the stored plan via
// rollback.Manager. All three honour the process-wide parallel-run cap and
// re-verify the plan artifact before touching any target.

package wiring

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/inventory"
	"github.com/nexus/levee/internal/lock"
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
func (e *Engine) newRunRunner() *engine.ClosureRunner {
	lockMgr := lock.NewLockManager(lock.NewLockStore(e.store), e.store)
	lockMgr.SetTTL(e.lockTTL)

	gateMgr := verify.NewGateManager()

	rollbackMgr := rollback.NewManager(
		rollback.WithWhitelistAll(),
		rollback.WithConcurrency(e.rollbackConcurrency),
		rollback.WithStopOnError(false),
	)

	batchCtrl := batch.NewController(
		batch.WithBatchErrorPolicy(batch.PolicyAbort),
		batch.WithTargetErrorPolicy(batch.PolicyAbort),
	)

	store := e.store
	return engine.NewClosureRunner(store, lockMgr, gateMgr, rollbackMgr, batchCtrl, nil,
		engine.WithHostGuard(func(ctx context.Context, hosts []string) error {
			return inventory.ValidateNotFrozen(ctx, store, hosts)
		}),
	)
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
	return e.executePlan(ctx, changeID, p)
}

// executePlan drives the full closure once and persists batch/step rows
// under the change's run id (operator-visible evidence). Callers own the
// parallel-run slot (see acquire); executePlan itself does not take one.
func (e *Engine) executePlan(ctx context.Context, changeID string, p *plan.Plan) (execRunID string, success bool, phase string, err error) {
	rx, err := newRunExec(ctx, e)
	if err != nil {
		return "", false, "", err
	}
	defer rx.close()

	// Fresh subsystem instances per execution (see newRunRunner).
	runner := e.newRunRunner()

	res, runErr := runner.Run(ctx, p, rx.executeFunc())
	if res == nil {
		// Only possible for an extraordinary rand failure in Run.
		return "", false, "", errors.Join(
			fmt.Errorf("wiring: closure run for change %q produced no result", changeID), runErr)
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

	execRunID, _, phase, err := e.executePlan(ctx, changeID, p)
	final := "failed"
	switch {
	case phase == string(engine.PhaseRolledBack):
		final = "rolled_back"
	case err == nil && phase == string(engine.PhaseCompleted):
		final = "completed"
	}
	run, gerr := e.store.GetRun(ctx, changeID)
	if gerr == nil && run != nil {
		run.Status = final
		run.UpdatedAt = utcNow()
		if uerr := e.store.UpdateRun(ctx, run); uerr != nil {
			err = errors.Join(err, fmt.Errorf("wiring: final status write: %w", uerr))
		}
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

	rx, err := newRunExec(ctx, e)
	if err != nil {
		return "", nil, err
	}
	defer rx.close()

	mgr := rollback.NewManager(
		rollback.WithWhitelistAll(),
		rollback.WithConcurrency(e.rollbackConcurrency),
		rollback.WithStopOnError(false),
	)
	res := mgr.Rollback(ctx, p, rx.executeFunc())

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
	hash := plan.ComputeHash(&p)
	if hash == "" || hash != run.PlanHash {
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
		ID:           p.ID,
		WorkflowName: p.WorkflowName,
		CreatedAt:    p.CreatedAt,
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
