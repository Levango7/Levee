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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/executor/modules/mysql"
	"github.com/nexus/levee/internal/executor/modules/pg"
	"github.com/nexus/levee/internal/executor/sqlguard"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
	"github.com/nexus/levee/internal/sqlreview"
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
	captureOK := false
	defer func() {
		if !captureOK {
			// A failed schema capture must not remain as an audit-only record that
			// a later restore could mistake for a usable baseline.
			_ = s.mgr.Store().Delete(context.WithoutCancel(ctx), snap.ID)
		}
	}()
	payloads := map[string]string{}
	if step.Module == "mysql" && step.Action == "query" {
		ch, target, err := s.rx.channelFor(ctx, target)
		if err != nil {
			return fmt.Errorf("channel for schema snapshot of %q: %w", target, err)
		}
		schemaPayloads, err := s.captureMySQLSchema(ctx, ch, target, step)
		if err != nil {
			return err
		}
		for key, value := range schemaPayloads {
			payloads[key] = value
		}
	}
	if step.Module == "pg" && step.Action == "query" {
		ch, target, err := s.rx.channelFor(ctx, target)
		if err != nil {
			return fmt.Errorf("channel for PostgreSQL schema snapshot of %q: %w", target, err)
		}
		schemaPayloads, err := capturePGSchema(ctx, ch, target, step)
		if err != nil {
			return err
		}
		for key, value := range schemaPayloads {
			payloads[key] = value
		}
	}
	if len(step.Rollback.SnapshotPaths) > 0 {
		ch, _, err := s.rx.channelFor(ctx, target)
		if err != nil {
			return fmt.Errorf("channel for snapshot of %q: %w", target, err)
		}
		filePayloads, err := s.fetchRemoteFiles(ctx, ch, step.Rollback.SnapshotPaths)
		if err != nil {
			return err
		}
		for key, value := range filePayloads {
			payloads[key] = value
		}
	}
	if len(payloads) == 0 {
		captureOK = true
		// Audit-only record: no payload, restore will be a no-op.
		log.Info("snapshot captured (audit-only, no paths)",
			"run_id", runID,
			"change_id", s.key,
			"target", target,
			"step", step.Name,
			"snapshot_id", snap.ID)
		return nil
	}
	if err := rollback.WriteSnapshotPayloads(snap.Path, payloads); err != nil {
		return fmt.Errorf("store snapshot payload for step %q on %q: %w", step.Name, target, err)
	}
	captureOK = true
	log.Info("snapshot captured",
		"run_id", runID,
		"change_id", s.key,
		"target", target,
		"step", step.Name,
		"files", len(payloads),
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
	// step name; the latest capture wins — a re-run supersedes).
	snap := latestSnapshotMatching(snaps, func(candidate *rollback.Snapshot) bool {
		name, _ := candidate.Metadata["step"].(string)
		return name == step.Name
	})
	if snap == nil {
		return fmt.Errorf("no snapshot recorded for step %q on %q (change %q)", step.Name, target, s.key)
	}

	payloads, err := rollback.ReadSnapshotPayloads(snap.Path)
	if err != nil {
		return fmt.Errorf("read snapshot %q: %w", snap.ID, err)
	}
	if isPGSchemaStep(step) && !hasPGSchemaPayload(payloads) {
		return fmt.Errorf("snapshot %q for pg.query step %q contains no restorable PostgreSQL DDL payload (legacy or audit-only record)", snap.ID, step.Name)
	}
	if len(payloads) == 0 {
		// Audit-only snapshot (no declared paths): nothing to restore.
		return nil
	}

	ch, targetDesc, err := s.rx.channelFor(ctx, target)
	if err != nil {
		return fmt.Errorf("channel for snapshot restore on %q: %w", target, err)
	}
	keys := make([]string, 0, len(payloads))
	for key := range payloads {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, origPath := range keys {
		content := payloads[origPath]
		if table, ok := strings.CutPrefix(origPath, pgSchemaPayloadPrefix); ok {
			args := cloneStepArgs(step.Args)
			args["table"] = table
			args["snapshot"] = content
			out, err := pg.New().Execute(ctx, "schema_restore", executor.ModuleInput{
				Action: "schema_restore", Args: args, Target: targetDesc, Channel: ch,
			})
			if err != nil {
				return fmt.Errorf("restore PostgreSQL schema %q on %q: %w", table, target, err)
			}
			if out.ExitCode != 0 {
				return fmt.Errorf("restore PostgreSQL schema %q on %q failed: %s", table, target, strings.TrimSpace(out.Stderr))
			}
			continue
		}
		if table, ok := strings.CutPrefix(origPath, mysqlSchemaPayloadPrefix); ok {
			args := cloneStepArgs(step.Args)
			args["table"] = table
			args["snapshot"] = content
			out, err := mysql.New().Execute(ctx, "schema_restore", executor.ModuleInput{
				Action: "schema_restore", Args: args, Target: targetDesc, Channel: ch,
			})
			if err != nil {
				return fmt.Errorf("restore schema %q on %q: %w", table, target, err)
			}
			if out.ExitCode != 0 {
				return fmt.Errorf("restore schema %q on %q failed: %s", table, target, strings.TrimSpace(out.Stderr))
			}
			continue
		}
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

// latestSnapshotMatching chooses the newest capture without relying on store
// iteration order. Snapshot IDs are random, so reverse filepath.Walk order is
// not a chronology. The ID tie-break makes equal timestamps deterministic; it
// does not claim to recover creation order within the same clock tick.
func latestSnapshotMatching(snaps []*rollback.Snapshot, matches func(*rollback.Snapshot) bool) *rollback.Snapshot {
	var latest *rollback.Snapshot
	for _, candidate := range snaps {
		if candidate == nil || !matches(candidate) {
			continue
		}
		if latest == nil || candidate.CreatedAt.After(latest.CreatedAt) ||
			(candidate.CreatedAt.Equal(latest.CreatedAt) && candidate.ID > latest.ID) {
			latest = candidate
		}
	}
	return latest
}

const (
	mysqlSchemaPayloadPrefix = "__mysql_schema__/"
	pgSchemaPayloadPrefix    = "__pg_schema__/"
)

func isPGSchemaStep(step plan.PlanStep) bool {
	return step.Module == "pg" && step.Action == "query"
}

func hasPGSchemaPayload(payloads map[string]string) bool {
	for key := range payloads {
		if strings.HasPrefix(key, pgSchemaPayloadPrefix) {
			return true
		}
	}
	return false
}

func cloneStepArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// captureMySQLSchema records one canonical CREATE statement per affected table.
// Only DDL-only steps are eligible: row changes are outside what a schema
// snapshot can restore, and a partial recovery must never call itself a full
// rollback. DML, DBDDL and unread statements therefore fail closed here.
func (s *remoteSnapshotter) captureMySQLSchema(ctx context.Context, ch channel.Channel, target channel.Target, step plan.PlanStep) (map[string]string, error) {
	out := map[string]string{}
	raw, _ := step.Args["sql"].(string)
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	res, err := sqlreview.Review(raw)
	if err != nil {
		return nil, fmt.Errorf("schema snapshot review: %w", err)
	}
	if res.Verdict == sqlreview.VerdictFail {
		return nil, fmt.Errorf("schema snapshot review failed for step %q", step.Name)
	}
	if res.Impact.Incomplete {
		return nil, fmt.Errorf("schema snapshot review is incomplete for step %q", step.Name)
	}
	var tables []string
	for _, statement := range res.Statements {
		switch statement.Kind {
		case sqlreview.KindInsert, sqlreview.KindUpdate, sqlreview.KindDelete:
			return nil, fmt.Errorf("schema restore supports DDL only; step %q also changes rows and cannot be recovered by a schema snapshot", step.Name)
		case sqlreview.KindDBDDL:
			return nil, fmt.Errorf("schema restore does not cover database-level DDL in step %q", step.Name)
		case sqlreview.KindDDL:
			tables = append(tables, statement.Impact.Writes...)
		}
	}
	tables = sqlguard.SortedUnique(tables)
	for _, table := range tables {
		args := cloneStepArgs(step.Args)
		args["tables"] = []any{table}
		capture, err := mysql.New().Execute(ctx, "schema_snapshot", executor.ModuleInput{
			Action: "schema_snapshot", Args: args, Target: target, Channel: ch,
		})
		if err != nil {
			return nil, fmt.Errorf("capture schema for table %q: %w", table, err)
		}
		if capture.ExitCode != 0 {
			return nil, fmt.Errorf("capture schema for table %q failed: %s", table, strings.TrimSpace(capture.Stderr))
		}
		out[mysqlSchemaPayloadPrefix+table] = strings.TrimSpace(capture.Stdout)
	}
	return out, nil
}

// capturePGSchema records a versioned pg_dump payload per affected table. It
// shares MySQL's DDL-only boundary: DML, DBDDL and unread statements cannot be
// called complete schema recoveries and fail before the apply starts.
func capturePGSchema(ctx context.Context, ch channel.Channel, target channel.Target, step plan.PlanStep) (map[string]string, error) {
	out := map[string]string{}
	raw, _ := step.Args["sql"].(string)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("PostgreSQL schema snapshot requires non-empty SQL in step %q", step.Name)
	}
	res, err := sqlreview.ReviewPostgres(raw)
	if err != nil {
		return nil, fmt.Errorf("PostgreSQL schema snapshot review: %w", err)
	}
	if res.Verdict == sqlreview.VerdictFail {
		return nil, fmt.Errorf("PostgreSQL schema snapshot review failed for step %q", step.Name)
	}
	if res.Impact.Incomplete {
		return nil, fmt.Errorf("PostgreSQL schema snapshot review is incomplete for step %q", step.Name)
	}
	var tables []string
	for _, statement := range res.Statements {
		switch statement.Kind {
		case sqlreview.KindInsert, sqlreview.KindUpdate, sqlreview.KindDelete:
			return nil, fmt.Errorf("PostgreSQL schema restore supports DDL only; step %q also changes rows and cannot be recovered by a schema snapshot", step.Name)
		case sqlreview.KindDBDDL:
			return nil, fmt.Errorf("PostgreSQL schema restore does not cover database-level DDL in step %q", step.Name)
		case sqlreview.KindDDL:
			tables = append(tables, statement.Impact.Writes...)
		}
	}
	tables = sqlguard.SortedUnique(tables)
	if len(tables) == 0 {
		return nil, fmt.Errorf("PostgreSQL schema snapshot found no affected table in step %q; an audit-only record is not a restorable baseline", step.Name)
	}
	for _, table := range tables {
		args := cloneStepArgs(step.Args)
		args["tables"] = []any{table}
		capture, err := pg.New().Execute(ctx, "schema_snapshot_ddl", executor.ModuleInput{
			Action: "schema_snapshot_ddl", Args: args, Target: target, Channel: ch,
		})
		if err != nil {
			return nil, fmt.Errorf("capture PostgreSQL schema for table %q: %w", table, err)
		}
		if capture.ExitCode != 0 {
			return nil, fmt.Errorf("capture PostgreSQL schema for table %q failed: %s", table, strings.TrimSpace(capture.Stderr))
		}
		var header struct {
			Table string `json:"table"`
		}
		if err := json.Unmarshal([]byte(capture.Stdout), &header); err != nil || header.Table == "" {
			return nil, fmt.Errorf("capture PostgreSQL schema for table %q returned an invalid versioned payload", table)
		}
		out[pgSchemaPayloadPrefix+header.Table] = capture.Stdout
	}
	return out, nil
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
