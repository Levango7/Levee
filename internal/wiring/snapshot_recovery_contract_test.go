package wiring

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/rollback"
)

func TestPostgresLegacyEmptySnapshotRollbackFailsClosed(t *testing.T) {
	vfs := newSnapVFS(nil)
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-pg", dir)
	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-pg")

	step := snapStep("alter-users")
	step.Module = "pg"
	step.Action = "query"
	step.Args = map[string]any{"sql": "ALTER TABLE users ADD COLUMN note text"}

	// New captures carry a versioned pg_dump payload. A pre-D-5 audit-only or
	// introspection-only record still cannot become a successful no-op restore.
	legacy := &rollback.Snapshot{
		ID: "snap-legacy", RunID: "chg-pg", Target: "web1", Type: rollback.SnapshotTypeFile,
		CreatedAt: time.Now().UTC(), Metadata: map[string]any{"step": step.Name},
	}
	legacy.Path = store.BuildPath(legacy.RunID, legacy.Target, legacy.ID)
	_, err = store.Create(context.Background(), legacy)
	require.NoError(t, err)
	err = s.RestoreForStep(context.Background(), "run-rollback", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "contains no restorable PostgreSQL DDL payload")
	assert.Empty(t, vfs.commands(), "the refusal happens before touching the target")
}

func TestSnapshotterRestoreSelectsNewestCaptureRegardlessOfIDOrder(t *testing.T) {
	vfs := newSnapVFS(nil)
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-latest", dir)
	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-latest")
	step := snapStep("push-conf", "/etc/app.conf")

	// filepath.Walk visits snap-a before snap-z. Reverse iteration therefore
	// used to select the OLDER snap-z payload; chronology, not the random ID,
	// must decide which retry baseline is restored.
	base := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id      string
		created time.Time
		content string
	}{
		{id: "snap-a-newest", created: base.Add(time.Second), content: "new-baseline"},
		{id: "snap-z-oldest", created: base, content: "old-baseline"},
	} {
		snap := &rollback.Snapshot{
			ID: fixture.id, RunID: "chg-latest", Target: "web1", Type: rollback.SnapshotTypeFile,
			CreatedAt: fixture.created, Metadata: map[string]any{"step": step.Name},
		}
		snap.Path = store.BuildPath(snap.RunID, snap.Target, snap.ID)
		_, err = store.Create(context.Background(), snap)
		require.NoError(t, err)
		require.NoError(t, rollback.WriteSnapshotPayloads(snap.Path, map[string]string{
			"/etc/app.conf": fixture.content,
		}))
	}

	require.NoError(t, s.RestoreForStep(context.Background(), "run-rollback", "web1", step))
	assert.Equal(t, "new-baseline", vfs.uploaded("/etc/app.conf"))
}

func TestLatestSnapshotMatchingBreaksTimestampTiesDeterministically(t *testing.T) {
	at := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	a := &rollback.Snapshot{ID: "snap-a", CreatedAt: at, Metadata: map[string]any{"step": "s"}}
	b := &rollback.Snapshot{ID: "snap-b", CreatedAt: at, Metadata: map[string]any{"step": "s"}}
	matches := func(candidate *rollback.Snapshot) bool {
		name, _ := candidate.Metadata["step"].(string)
		return name == "s"
	}
	assert.Same(t, b, latestSnapshotMatching([]*rollback.Snapshot{a, b}, matches))
	assert.Same(t, b, latestSnapshotMatching([]*rollback.Snapshot{b, a}, matches))
}
