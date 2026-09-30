// run_snapshotter.go implements the run-level baseline over the same channel
// machinery as the step-level snapshotter.
//
// It embeds *remoteSnapshotter rather than duplicating it: the wire protocol
// for pulling a file off a target and pushing it back is identical, and two
// copies of that would drift. What differs is cardinality (once per run, not
// once per step), the metadata the snapshot record is tagged with (scope=run
// instead of a step name, which is also how restore finds it), and the
// failure semantics enforced by the engine (fail-closed, see
// engine.RunSnapshotter).
//
// Keying is inherited unchanged: records are stored under the CHANGE id, so a
// retry's baseline supersedes the earlier one and the manual RollbackChange
// path — which knows only the change — can still find it.

package wiring

import (
	"context"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/rollback"
)

// runSnapshotMetadataKey tags a snapshot record as belonging to the run-level
// baseline. Step-level records tag the step name instead, so the two can
// share one store and one List() without colliding.
const runSnapshotMetadataKey = "scope"

// runSnapshotScopeValue is the only value stored under that key.
const runSnapshotScopeValue = "run"

type runRemoteSnapshotter struct {
	*remoteSnapshotter
}

// Compile-time assertion: the run-level snapshotter satisfies the engine hook.
var _ engine.RunSnapshotter = (*runRemoteSnapshotter)(nil)

func newRunRemoteSnapshotter(base *remoteSnapshotter) *runRemoteSnapshotter {
	return &runRemoteSnapshotter{remoteSnapshotter: base}
}

// snapshotTypeFor maps the declaration's type onto the store's capture
// semantics. An omitted type means file, matching the hash's projection of an
// omitted type — the plan and the capture must agree on what "no type" means.
func snapshotTypeFor(spec *dsl.RunSnapshotSpec) rollback.SnapshotType {
	if spec != nil && spec.Type == "config" {
		return rollback.SnapshotTypeConfig
	}
	return rollback.SnapshotTypeFile
}

// CaptureRun records the baseline on every target, once, before the first
// batch. Targets are captured sequentially for the same reason the step-level
// capture is: correctness does not depend on concurrency, and a sequential
// capture fails fast on the first unreachable target instead of leaving a
// half-populated baseline set behind.
func (s *runRemoteSnapshotter) CaptureRun(ctx context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error {
	if spec == nil {
		return nil
	}
	for _, target := range targets {
		snap, err := s.mgr.CreateSnapshot(ctx, s.key, target, nil, snapshotTypeFor(spec), map[string]any{
			runSnapshotMetadataKey: runSnapshotScopeValue,
			"paths":                len(spec.Paths),
		})
		if err != nil {
			return fmt.Errorf("create run baseline record for %q: %w", target, err)
		}
		ch, _, err := s.rx.channelFor(ctx, target)
		if err != nil {
			return fmt.Errorf("channel for run baseline of %q: %w", target, err)
		}
		payloads, err := s.fetchRemoteFiles(ctx, ch, spec.Paths)
		if err != nil {
			return err
		}
		if err := rollback.WriteSnapshotPayloads(snap.Path, payloads); err != nil {
			return fmt.Errorf("store run baseline payload for %q: %w", target, err)
		}
		log.Info("run baseline captured",
			"run_id", runID,
			"change_id", s.key,
			"target", target,
			"files", len(payloads),
			"snapshot_id", snap.ID)
	}
	return nil
}

// RestoreRun writes the baseline back on every target. Fails closed: a target
// with no run-level record is an error, not a skip. A silently unrestored
// target is precisely the state the declaration promised to make
// recoverable, and the operator would have no way to learn it did not happen.
func (s *runRemoteSnapshotter) RestoreRun(ctx context.Context, runID string, targets []string, spec *dsl.RunSnapshotSpec) error {
	if spec == nil {
		return nil
	}
	var firstErr error
	for _, target := range targets {
		if err := s.restoreTarget(ctx, runID, target); err != nil {
			// Keep going: restoring the other targets is strictly better than
			// stopping at the first failure, and the caller reports the first
			// error. Half a restore is bad; a third of a restore is worse.
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *runRemoteSnapshotter) restoreTarget(ctx context.Context, runID, target string) error {
	snaps, err := s.mgr.Store().List(ctx, s.key, target)
	if err != nil {
		return fmt.Errorf("list run baselines for %q/%q: %w", s.key, target, err)
	}
	var snap *rollback.Snapshot
	for i := len(snaps) - 1; i >= 0; i-- {
		if scope, _ := snaps[i].Metadata[runSnapshotMetadataKey].(string); scope == runSnapshotScopeValue {
			snap = snaps[i]
			break
		}
	}
	if snap == nil {
		return fmt.Errorf("no run-level baseline recorded for %q (change %q)", target, s.key)
	}
	payloads, err := rollback.ReadSnapshotPayloads(snap.Path)
	if err != nil {
		return fmt.Errorf("read run baseline %q: %w", snap.ID, err)
	}
	if len(payloads) == 0 {
		return nil
	}
	ch, _, err := s.rx.channelFor(ctx, target)
	if err != nil {
		return fmt.Errorf("channel for run baseline restore on %q: %w", target, err)
	}
	for origPath, content := range payloads {
		if err := ch.Upload(ctx, origPath, strings.NewReader(content)); err != nil {
			return fmt.Errorf("restore run baseline %q on %q: %w", origPath, target, err)
		}
	}
	log.Info("run baseline restored",
		"run_id", runID,
		"change_id", s.key,
		"target", target,
		"files", len(payloads),
		"snapshot_id", snap.ID)
	return nil
}
