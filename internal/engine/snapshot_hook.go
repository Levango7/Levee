// snapshot_hook.go wires pre-apply target snapshots into the closure
// (design §4.4.4.2 "快照创建": apply 前对每台目标机创建快照，快照创建
// 失败则该目标机不进 apply；§4.4.6.3 快照是回滚的依据).
//
// The ClosureRunner stays transport-agnostic: it does not know how to
// reach a target or where snapshots live. A Snapshotter captures the
// files a step declared (RollbackSpec.SnapshotPaths, strategy "snapshot")
// into whatever backend the wiring layer configured, and RestoreSnapshot
// writes them back during rollback. The default (nil) snapshotter is a
// no-op: workflows that declare strategy "snapshot" but run under a
// wiring that did not install a snapshotter simply skip the capture, and
// the rollback half sees no snapshot to restore — exactly the
// built-but-unwired behaviour this hook exists to replace once wiring
// installs a real implementation.

package engine

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
)

// Snapshotter captures and restores target-machine state for a plan step
// whose RollbackSpec.Strategy is "snapshot". Implementations are
// transport-aware (the wiring layer supplies the channel); the engine
// only coordinates WHEN capture/restore happens.
//
// All methods must be safe for concurrent use: the batch controller may
// drive several targets in parallel.
type Snapshotter interface {
	// CaptureForStep records a snapshot of step on target under runID.
	// It is called once per (runID, target, step) BEFORE the step
	// executes. A non-nil error aborts the run before any mutation of
	// that target (the whole run, in the abort policy). It must be a
	// no-op (returning nil) for steps that declare no snapshot payload.
	CaptureForStep(ctx context.Context, runID, target string, step plan.PlanStep) error

	// RestoreForStep restores the snapshot recorded for step on target.
	// It is called by the rollback path (see manager.go) instead of
	// running undo steps when the step's rollback strategy is
	// "snapshot". A non-nil error marks that rollback step failed.
	RestoreForStep(ctx context.Context, runID, target string, step plan.PlanStep) error
}

// WithSnapshotter installs the pre-apply snapshot coordinator. It runs
// after lock acquisition and before the first batch executes (and after
// the host guard), so a snapshot failure aborts with all locks released
// and zero mutation. Omitting it leaves snapshot capture/restore
// disabled (the pre-wiring no-op behaviour).
func WithSnapshotter(s Snapshotter) ClosureOption {
	return func(cr *ClosureRunner) { cr.snapshotter = s }
}

// SetSnapshotter attaches (or detaches, with nil) the snapshot coordinator
// after construction. The wiring layer creates the runExec (its channel
// cache) before assembling the runner, and the snapshotter needs both — so
// the runner is constructed first and the hook attached right after, always
// before Run. The runner is single-flight, so no lock is needed.
func (cr *ClosureRunner) SetSnapshotter(s Snapshotter) {
	cr.snapshotter = s
}

// captureSnapshots drives the pre-apply capture over every (target, step)
// pair whose rollback strategy is "snapshot". It is a no-op when no
// snapshotter is installed. The first capture error aborts the run.
//
// Iteration is sequential here (capture precedes the concurrent batch
// execution and is itself cheap: file copies over an already-dialled
// channel); correctness does not depend on concurrency.
func (cr *ClosureRunner) captureSnapshots(ctx context.Context, runID string, p *plan.Plan) error {
	if cr.snapshotter == nil {
		return nil
	}
	for _, b := range p.Batches {
		for _, target := range b.Targets {
			for _, step := range b.Steps {
				if !isSnapshotStep(step) {
					continue
				}
				if err := cr.snapshotter.CaptureForStep(ctx, runID, target, step); err != nil {
					return fmt.Errorf("target %s step %s: %w", target, step.Name, err)
				}
			}
		}
	}
	return nil
}

// isSnapshotStep reports whether a step declares snapshot-based rollback
// (RollbackSpec.Strategy == "snapshot"). Steps with no rollback spec or a
// different strategy are skipped: undo-action / config-revert rollback
// uses the undo-step machinery in manager.go, not snapshots.
func isSnapshotStep(step plan.PlanStep) bool {
	return step.Rollback != nil && step.Rollback.Strategy == "snapshot"
}

// --- run-level baseline -----------------------------------------------------

// RunSnapshotter captures and restores the RUN-level baseline: one capture of
// the declared paths on every target before the first batch, one restore if
// the run rolls back.
//
// It is a separate interface from Snapshotter on purpose. The two have
// different cardinality (once per (target, step) vs once per run), different
// cardinality of restore (per step, inside the manager's compensation walk vs
// once, after it), and — most importantly — different failure semantics. A
// missing step snapshotter is a no-op by design, because that is the
// pre-wiring behaviour. A missing run snapshotter is NOT: the plan declared a
// baseline, so running without it would mutate targets whose pre-state was
// never recorded and leave the operator believing rollback can restore them.
// Declared-but-unwired therefore aborts the run before any mutation.
type RunSnapshotter interface {
	// CaptureRun records the baseline on every target under runID, before
	// the first batch executes. A non-nil error aborts the run.
	CaptureRun(ctx context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error

	// RestoreRun writes the baseline back over every target. It is called
	// once, after the step compensations have run, so that the run's
	// pre-state wins for the paths it covers.
	RestoreRun(ctx context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error
}

// WithRunSnapshotter installs the run-level baseline coordinator.
func WithRunSnapshotter(s RunSnapshotter) ClosureOption {
	return func(cr *ClosureRunner) { cr.runSnapshotter = s }
}

// SetRunSnapshotter attaches (or detaches, with nil) the run-level baseline
// coordinator after construction, mirroring SetSnapshotter: the wiring layer
// builds the transport after the runner exists.
func (cr *ClosureRunner) SetRunSnapshotter(s RunSnapshotter) {
	cr.runSnapshotter = s
}

// captureSnapshotsAndBaseline runs both pre-apply capture halves and returns
// a closure-ready error. The step-level capture comes first: a plan that
// declares per-step snapshots has the narrower baseline, and recording the
// run-level one first would mean a step-level capture failure leaves an
// orphan run record behind.
func (cr *ClosureRunner) captureSnapshotsAndBaseline(ctx context.Context, runID string, p *plan.Plan, targets []string) error {
	if err := cr.captureSnapshots(ctx, runID, p); err != nil {
		return fmt.Errorf("closure: pre-apply snapshot: %w", err)
	}
	if err := cr.captureRunSnapshot(ctx, runID, p, targets); err != nil {
		return fmt.Errorf("closure: run-level snapshot: %w", err)
	}
	return nil
}

// captureRunSnapshot takes the run-level baseline. It is a no-op for plans
// that declare none, and a hard failure for plans that declare one while no
// RunSnapshotter is installed — see the interface comment for why this is
// the opposite of the step-level hook.
func (cr *ClosureRunner) captureRunSnapshot(ctx context.Context, runID string, p *plan.Plan, targets []string) error {
	if p == nil || p.RunSnapshot == nil {
		return nil
	}
	if cr.runSnapshotter == nil {
		return fmt.Errorf("plan declares a run-level snapshot baseline (%d path(s), scope %q) "+
			"but no run snapshotter is installed: the baseline cannot be recorded, so the run "+
			"is refused before any target is touched rather than executed with an unrecorded "+
			"pre-state (configure --engine-snapshot-dir)",
			len(p.RunSnapshot.Paths), p.RunSnapshot.Scope)
	}
	if err := cr.runSnapshotter.CaptureRun(ctx, runID, targets, p.RunSnapshot); err != nil {
		return fmt.Errorf("run-level snapshot capture: %w", err)
	}
	return nil
}

// restoreRunBaseline writes the run-level baseline back after the
// compensations ran, recording any failure on the result instead of
// returning it. Two deliberate choices:
//
//   - It does NOT change the run's verdict. The compensation ledger owns that
//     (PhaseRolledBack / PhasePartialRollback), and a failed baseline restore
//     must not rewrite a completed rollback into a failed run. The operator
//     gets a dedicated field plus an error log instead.
//   - It runs on a background context, exactly like the rollback dispatch in
//     Run: cancellation is one of the ways rollback gets triggered, and
//     stopping mid-restore would leave the baseline half-written, which is
//     worse than not restoring at all.
func (cr *ClosureRunner) restoreRunBaseline(result *ClosureResult, p *plan.Plan, targets []string) {
	if result == nil {
		return
	}
	if err := cr.restoreRunSnapshot(context.Background(), result.RunID, p, targets); err != nil {
		result.RunSnapshotRestoreError = err
		log.Error("run-level snapshot restore failed; baseline may not be back on the targets",
			"run_id", result.RunID,
			"error", err)
	}
}

// restoreRunSnapshot writes the baseline back after the compensations ran.
// A restore failure is reported to the caller so it can be surfaced in the
// run result; it never rewrites the rollback verdict, which belongs to the
// compensation ledger.
func (cr *ClosureRunner) restoreRunSnapshot(ctx context.Context, runID string, p *plan.Plan, targets []string) error {
	if p == nil || p.RunSnapshot == nil || cr.runSnapshotter == nil {
		return nil
	}
	return cr.runSnapshotter.RestoreRun(ctx, runID, targets, p.RunSnapshot)
}
