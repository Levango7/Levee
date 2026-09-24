// snapshotter.go is the channel-aware snapshot coordinator: it implements
// engine.Snapshotter for the wiring layer, pulling declared files from the
// TARGET machine over the run's cached channel (cat | base64) into the
// rollback.SnapshotManager store, and pushing them back on restore.
//
// The pre-existing SnapshotManager only knew local-FS paths (os.Stat +
// copyFile on the master): wiring it naively would have snapshotted the
// LEVEE server's own filesystem instead of the target's. This adapter is
// the transport half the manager was missing — the manager keeps the
// record lifecycle (create meta, delete on failure, paths.json mapping)
// and this adapter fills the contents from the remote side.
//
// Design notes:
//   - Capture writes base64 payloads into files/<flattened-path> under the
//     snapshot directory the store created, plus a paths.json identical in
//     shape to the manager's own (flattened -> ORIGINAL TARGET PATH). The
//     manager therefore never needs to learn about remote paths.
//   - Restore reads the captured files back and Uploads them to the target
//     over the same cached channel (path traversal is not possible: the
//     restore path list is exactly what paths.json recorded at capture).
//   - Files are captured per (runID, target, step): the snapshot metadata
//     records the step name so operators can trace which step declared
//     which backup.

package wiring

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
)

// remoteSnapshotter implements engine.Snapshotter over a runExec's cached
// channels and a rollback.SnapshotManager.
//
// Keying: snapshots are stored under the CHANGE id (s.key), not the
// closure run id the engine passes in. The closure run id is minted per
// execution (a retry runs under a new one), while snapshots must survive
// across paths that only know the change: the automatic closure rollback
// (which re-receives its run id), the manual RollbackChange RPC (which
// knows only the change id) and post-completion undos. Restore picks the
// LAST capture per (key, target, step), so a re-run supersedes the
// earlier capture — restore always returns the target to the pre-state
// of the most recent execution, which is the semantics operators expect.
type remoteSnapshotter struct {
	rx   *runExec
	mgr  *rollback.SnapshotManager
	key  string
	kind rollback.SnapshotType
}

// Compile-time assertion: the wiring snapshotter satisfies the engine hook.
var _ engine.Snapshotter = (*remoteSnapshotter)(nil)

// newRemoteSnapshotter returns a snapshotter capturing remote files via rx
// into mgr's store, keyed by the change id (see the Keying note above).
func newRemoteSnapshotter(rx *runExec, mgr *rollback.SnapshotManager, changeID string) *remoteSnapshotter {
	return &remoteSnapshotter{rx: rx, mgr: mgr, key: changeID, kind: rollback.SnapshotTypeFile}
}

// CaptureForStep pulls each declared SnapshotPaths entry from the target
// and stores it in the snapshot store under the change key. Steps
// declaring no paths produce an audit-only snapshot record (empty
// contents, restore no-op), mirroring the local manager semantics. A
// capture error aborts the run (the closure's abort policy: 快照创建
// 失败则该目标机不进 apply).
func (s *remoteSnapshotter) CaptureForStep(ctx context.Context, runID, target string, step plan.PlanStep) error {
	if step.Rollback == nil {
		return nil // not a snapshot step; the engine filters these already
	}
	// The manager creates the record (and its directory); the contents
	// are then filled from the remote side via the payload API.
	snap, err := s.mgr.CreateSnapshot(ctx, s.key, target, nil, s.kind, map[string]any{
		"step": step.Name,
	})
	if err != nil {
		return fmt.Errorf("create snapshot record for step %q on %q: %w", step.Name, target, err)
	}
	if len(step.Rollback.SnapshotPaths) == 0 {
		// Audit-only record: no payload, restore will be a no-op.
		log.Info("snapshot captured (audit-only, no paths)",
			"run_id", runID,
			"change_id", s.key,
			"target", target,
			"step", step.Name,
			"snapshot_id", snap.ID)
		return nil
	}

	ch, _, err := s.rx.channelFor(ctx, target)
	if err != nil {
		return fmt.Errorf("channel for snapshot of %q: %w", target, err)
	}
	payloads, err := s.fetchRemoteFiles(ctx, ch, step.Rollback.SnapshotPaths)
	if err != nil {
		return err
	}
	if err := rollback.WriteSnapshotPayloads(snap.Path, payloads); err != nil {
		return fmt.Errorf("store snapshot payload for step %q on %q: %w", step.Name, target, err)
	}
	log.Info("snapshot captured",
		"run_id", runID,
		"change_id", s.key,
		"target", target,
		"step", step.Name,
		"files", len(step.Rollback.SnapshotPaths),
		"snapshot_id", snap.ID)
	return nil
}

// RestoreForStep pushes the captured files back to the target. Lookup goes
// through the change key (see the Keying note on the struct), NOT the
// passed runID — the manual RollbackChange path passes its own execution
// id, which never captured anything. Fails closed: no snapshot for the
// (change, target, step) → error (operators must see an unrestorable
// step, never a silent skip).
func (s *remoteSnapshotter) RestoreForStep(ctx context.Context, runID, target string, step plan.PlanStep) error {
	snaps, err := s.mgr.Store().List(ctx, s.key, target)
	if err != nil {
		return fmt.Errorf("list snapshots for %q/%q: %w", s.key, target, err)
	}
	// Find the snapshot recorded for this step (metadata carries the
	// step name; the last capture wins — a re-run supersedes).
	var snap *rollback.Snapshot
	for i := len(snaps) - 1; i >= 0; i-- {
		if name, _ := snaps[i].Metadata["step"].(string); name == step.Name {
			snap = snaps[i]
			break
		}
	}
	if snap == nil {
		return fmt.Errorf("no snapshot recorded for step %q on %q (change %q)", step.Name, target, s.key)
	}

	payloads, err := rollback.ReadSnapshotPayloads(snap.Path)
	if err != nil {
		return fmt.Errorf("read snapshot %q: %w", snap.ID, err)
	}
	if len(payloads) == 0 {
		// Audit-only snapshot (no declared paths): nothing to restore.
		return nil
	}

	ch, _, err := s.rx.channelFor(ctx, target)
	if err != nil {
		return fmt.Errorf("channel for snapshot restore on %q: %w", target, err)
	}
	for origPath, content := range payloads {
		if err := ch.Upload(ctx, origPath, strings.NewReader(content)); err != nil {
			return fmt.Errorf("restore %q on %q: %w", origPath, target, err)
		}
	}
	log.Info("snapshot restored",
		"run_id", runID,
		"change_id", s.key,
		"target", target,
		"step", step.Name,
		"files", len(payloads),
		"snapshot_id", snap.ID)
	return nil
}

// fetchRemoteFiles pulls each path's contents from the target as base64
// text (one command per file). Paths come from reviewed workflow
// declarations, but they are still single-quoted for the remote shell;
// embedded single quotes use the standard '\” escape.
func (s *remoteSnapshotter) fetchRemoteFiles(ctx context.Context, ch channel.Channel, paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		res, err := ch.Exec(ctx, "base64 "+shellQuoteSingle(p))
		if err != nil {
			return nil, fmt.Errorf("capture %q: %w", p, err)
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("capture %q: exit %d: %s", p, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
		if err != nil {
			return nil, fmt.Errorf("capture %q: decode: %w", p, err)
		}
		out[p] = string(decoded)
	}
	return out, nil
}

// shellQuoteSingle wraps s in single quotes for a POSIX remote shell,
// escaping embedded single quotes with the '\” sequence.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
