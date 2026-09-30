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
