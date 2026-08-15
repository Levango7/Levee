package rollback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test fixtures ---------------------------------------------------------

// newTestStore returns a FileSnapshotStore rooted at a fresh temp dir. The
// t.Cleanup hook removes the temp dir when the test finishes.
func newTestStore(t *testing.T) *FileSnapshotStore {
	t.Helper()
	dir := t.TempDir()
	store, err := NewFileSnapshotStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return store
}

// writeFile writes content to path, creating parent dirs as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

// readFile reads path, failing the test on error.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// --- NewFileSnapshotStore --------------------------------------------------

func TestNewFileSnapshotStoreCreatesRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "snapstore")
	store, err := NewFileSnapshotStore(dir)
	require.NoError(t, err)
	assert.DirExists(t, dir)
	assert.Equal(t, dir, store.RootDir())
}

func TestNewFileSnapshotStoreEmptyRootRejected(t *testing.T) {
	_, err := NewFileSnapshotStore("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root dir is empty")
}

// --- CreateSnapshot --------------------------------------------------------

func TestCreateSnapshotFile(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	// Create a source file to snapshot.
	srcDir := filepath.Join(t.TempDir(), "src")
	srcFile := filepath.Join(srcDir, "app.conf")
	writeFile(t, srcFile, "original-content")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{srcFile}, SnapshotTypeFile, map[string]any{"step": "deploy"})
	require.NoError(t, err)

	assert.NotEmpty(t, snap.ID)
	assert.Equal(t, "run-1", snap.RunID)
	assert.Equal(t, "host-a", snap.Target)
	assert.Equal(t, SnapshotTypeFile, snap.Type)
	assert.NotEmpty(t, snap.Path)
	assert.False(t, snap.CreatedAt.IsZero())
	assert.Equal(t, "deploy", snap.Metadata["step"])

	// The snapshot directory should exist with files/ and configs/ sub-dirs.
	assert.DirExists(t, snap.Path)
	assert.DirExists(t, filepath.Join(snap.Path, "files"))
	assert.DirExists(t, filepath.Join(snap.Path, "configs"))

	// The meta.json should be on disk.
	metaData, err := os.ReadFile(filepath.Join(snap.Path, "meta.json"))
	require.NoError(t, err)
	assert.Contains(t, string(metaData), snap.ID)
}

func TestCreateSnapshotConfigType(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store, WithSnapshotType(SnapshotTypeConfig))
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, srcFile, "key: value")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{srcFile}, "", nil)
	require.NoError(t, err)
	assert.Equal(t, SnapshotTypeConfig, snap.Type, "empty type should use manager default")

	// Config snapshots store contents under configs/.
	stored := filepath.Join(snap.Path, "configs", flattenPath(srcFile))
	assert.FileExists(t, stored)
	assert.Equal(t, "key: value", readFile(t, stored))
}

func TestCreateSnapshotDefaultTypeFile(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f.txt")
	writeFile(t, srcFile, "x")

	snap, err := mgr.CreateSnapshot(context.Background(), "r", "h",
		[]string{srcFile}, "", nil)
	require.NoError(t, err)
	assert.Equal(t, SnapshotTypeFile, snap.Type, "default type should be file")
}

func TestCreateSnapshotEmptyPaths(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	// Empty paths: snapshot record is still created.
	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		nil, SnapshotTypeFile, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, snap.ID)
	assert.DirExists(t, snap.Path)
}

func TestCreateSnapshotMultiplePaths(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	writeFile(t, p1, "AAA")
	writeFile(t, p2, "BBB")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{p1, p2}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	// Both files should be in the snapshot's files/ dir.
	stored1 := filepath.Join(snap.Path, "files", flattenPath(p1))
	stored2 := filepath.Join(snap.Path, "files", flattenPath(p2))
	assert.Equal(t, "AAA", readFile(t, stored1))
	assert.Equal(t, "BBB", readFile(t, stored2))
}

func TestCreateSnapshotDirectory(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcDir := filepath.Join(t.TempDir(), "mydir")
	writeFile(t, filepath.Join(srcDir, "f1.txt"), "one")
	writeFile(t, filepath.Join(srcDir, "sub", "f2.txt"), "two")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{srcDir}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	// The directory tree should be preserved under files/<flattened-dir>.
	storedDir := filepath.Join(snap.Path, "files", flattenPath(srcDir))
	assert.Equal(t, "one", readFile(t, filepath.Join(storedDir, "f1.txt")))
	assert.Equal(t, "two", readFile(t, filepath.Join(storedDir, "sub", "f2.txt")))
}

func TestCreateSnapshotNonExistentPath(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err = mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{missing}, SnapshotTypeFile, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat")

	// The snapshot should be cleaned up on failure.
	snaps, err := store.List(context.Background(), "run-1", "host-a")
	require.NoError(t, err)
	assert.Empty(t, snaps, "failed snapshot should be deleted")
}

func TestCreateSnapshotEmptyRunIDRejected(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	_, err = mgr.CreateSnapshot(context.Background(), "", "host-a", nil, SnapshotTypeFile, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runID is empty")
}

func TestCreateSnapshotEmptyTargetRejected(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	_, err = mgr.CreateSnapshot(context.Background(), "run-1", "", nil, SnapshotTypeFile, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target is empty")
}

func TestCreateSnapshotCancelledContext(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = mgr.CreateSnapshot(ctx, "run-1", "host-a", nil, SnapshotTypeFile, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// --- RestoreSnapshot -------------------------------------------------------

func TestRestoreSnapshotRoundTrip(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	// Create a source file, snapshot it, then modify the source, then
	// restore and verify the original content is back.
	srcFile := filepath.Join(t.TempDir(), "app.conf")
	writeFile(t, srcFile, "original")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	// Mutate the source.
	writeFile(t, srcFile, "modified")
	assert.Equal(t, "modified", readFile(t, srcFile))

	// Restore.
	restored, err := mgr.RestoreSnapshot(context.Background(), snap.ID)
	require.NoError(t, err)
	assert.Equal(t, snap.ID, restored.ID)

	// The source should be back to original.
	assert.Equal(t, "original", readFile(t, srcFile))
}

func TestRestoreSnapshotConfigRoundTrip(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store, WithSnapshotType(SnapshotTypeConfig))
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, srcFile, "version: 1")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{srcFile}, "", nil)
	require.NoError(t, err)

	writeFile(t, srcFile, "version: 2")
	_, err = mgr.RestoreSnapshot(context.Background(), snap.ID)
	require.NoError(t, err)

	assert.Equal(t, "version: 1", readFile(t, srcFile))
}

func TestRestoreSnapshotMultipleFiles(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	writeFile(t, p1, "A1")
	writeFile(t, p2, "B1")

	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		[]string{p1, p2}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	// Mutate both.
	writeFile(t, p1, "A2")
	writeFile(t, p2, "B2")

	_, err = mgr.RestoreSnapshot(context.Background(), snap.ID)
	require.NoError(t, err)

	assert.Equal(t, "A1", readFile(t, p1))
	assert.Equal(t, "B1", readFile(t, p2))
}

func TestRestoreSnapshotEmptyContents(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	// Snapshot with no paths → empty contents → restore is a no-op.
	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		nil, SnapshotTypeFile, nil)
	require.NoError(t, err)

	restored, err := mgr.RestoreSnapshot(context.Background(), snap.ID)
	require.NoError(t, err)
	assert.Equal(t, snap.ID, restored.ID)
}

func TestRestoreSnapshotNonExistentID(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	_, err = mgr.RestoreSnapshot(context.Background(), "snap-nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restore snapshot: get")
}

func TestRestoreSnapshotEmptyIDRejected(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	_, err = mgr.RestoreSnapshot(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ID is empty")
}

func TestRestoreSnapshotCancelledContext(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = mgr.RestoreSnapshot(ctx, "any")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// --- SnapshotStore Get / List / Delete -------------------------------------

func TestStoreGetAfterCreate(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f")
	writeFile(t, srcFile, "x")
	snap, err := mgr.CreateSnapshot(context.Background(), "r", "h",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	got, err := store.Get(context.Background(), snap.ID)
	require.NoError(t, err)
	assert.Equal(t, snap.ID, got.ID)
	assert.Equal(t, snap.RunID, got.RunID)
	assert.Equal(t, snap.Target, got.Target)
}

func TestStoreGetNonExistent(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get(context.Background(), "nope")
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestStoreGetEmptyID(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ID is empty")
}

func TestStoreListByRunID(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f")
	writeFile(t, srcFile, "x")

	_, err = mgr.CreateSnapshot(context.Background(), "run-A", "host-1",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)
	_, err = mgr.CreateSnapshot(context.Background(), "run-A", "host-2",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)
	_, err = mgr.CreateSnapshot(context.Background(), "run-B", "host-1",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	// List by run-A (any target).
	snaps, err := store.List(context.Background(), "run-A", "")
	require.NoError(t, err)
	assert.Len(t, snaps, 2)

	// List by run-A + host-1.
	snaps, err = store.List(context.Background(), "run-A", "host-1")
	require.NoError(t, err)
	assert.Len(t, snaps, 1)
	assert.Equal(t, "host-1", snaps[0].Target)
}

func TestStoreListAll(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f")
	writeFile(t, srcFile, "x")

	for i := 0; i < 3; i++ {
		_, err := mgr.CreateSnapshot(context.Background(), "r", "h",
			[]string{srcFile}, SnapshotTypeFile, nil)
		require.NoError(t, err)
	}

	snaps, err := store.List(context.Background(), "", "")
	require.NoError(t, err)
	assert.Len(t, snaps, 3)
}

func TestStoreDelete(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f")
	writeFile(t, srcFile, "x")
	snap, err := mgr.CreateSnapshot(context.Background(), "r", "h",
		[]string{srcFile}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	snapDir := snap.Path
	assert.DirExists(t, snapDir)

	require.NoError(t, store.Delete(context.Background(), snap.ID))
	assert.NoDirExists(t, snapDir, "delete should remove the snapshot directory")

	// Get should now fail.
	_, err = store.Get(context.Background(), snap.ID)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestStoreDeleteIdempotent(t *testing.T) {
	store := newTestStore(t)
	err := store.Delete(context.Background(), "nonexistent")
	require.NoError(t, err, "deleting a non-existent snapshot should be idempotent")
}

func TestStoreDeleteEmptyID(t *testing.T) {
	store := newTestStore(t)
	err := store.Delete(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ID is empty")
}

// --- path flattening -------------------------------------------------------

func TestFlattenPathAbsolute(t *testing.T) {
	if runtime.GOOS == "windows" {
		// On Windows both the drive colon and the separator become
		// underscores: "C:\etc\..." → "rootC__etc_...".
		assert.Equal(t, "rootC__etc_nginx_nginx.conf", flattenPath(`C:\etc\nginx\nginx.conf`))
	} else {
		assert.Equal(t, "root_etc_nginx_nginx.conf", flattenPath("/etc/nginx/nginx.conf"))
	}
}

func TestFlattenPathRelative(t *testing.T) {
	assert.Equal(t, "configs_app.yml", flattenPath(filepath.Join("configs", "app.yml")))
}

func TestUnflattenPathAbsolute(t *testing.T) {
	// unflattenPath is a best-effort fallback; on Windows it cannot
	// recover the drive letter. We only assert exact behaviour on Unix.
	if runtime.GOOS == "windows" {
		result := unflattenPath("rootC__etc_nginx_nginx.conf")
		assert.NotEmpty(t, result)
		assert.Contains(t, result, "etc")
	} else {
		assert.Equal(t, "/etc/nginx/nginx.conf", unflattenPath("root_etc_nginx_nginx.conf"))
	}
}

func TestUnflattenPathRelative(t *testing.T) {
	assert.Equal(t, filepath.Join("configs", "app.yml"), unflattenPath("configs_app.yml"))
}

func TestFlattenUnflattenRoundTripAbsolute(t *testing.T) {
	// flattenPath is not perfectly invertible (it replaces separators and
	// colons with underscores, and unflattenPath cannot distinguish an
	// original underscore from a replaced separator). RestoreSnapshot
	// uses the paths.json mapping for exact round-trips; unflattenPath is
	// only a best-effort fallback. On Unix (where paths have no colons)
	// we can verify flatten ∘ unflatten ∘ flatten == flatten; on Windows
	// the drive-letter ambiguity breaks this identity, so we skip.
	if runtime.GOOS == "windows" {
		t.Skip("unflattenPath round-trip is not exact on Windows; restore uses paths.json")
	}
	orig := "/etc/nginx/nginx.conf"
	flat := flattenPath(orig)
	back := unflattenPath(flat)
	reflat := flattenPath(back)
	assert.Equal(t, flat, reflat, "flatten(unflatten(flatten(p))) should equal flatten(p)")
}

func TestFlattenUnflattenRoundTripRelative(t *testing.T) {
	orig := filepath.Join("configs", "app.yml")
	flat := flattenPath(orig)
	back := unflattenPath(flat)
	assert.Equal(t, filepath.Clean(orig), filepath.Clean(back))
}

// --- NewSnapshotManager ----------------------------------------------------

func TestNewSnapshotManagerNilStoreRejected(t *testing.T) {
	_, err := NewSnapshotManager(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store is nil")
}

func TestSnapshotManagerStoreAccessor(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)
	assert.Equal(t, store, mgr.Store())
}

// --- in-memory store (interface compliance) --------------------------------

// memStore is a minimal in-memory SnapshotStore used to verify the interface
// contract and to test SnapshotManager against a non-File backend.
type memStore struct {
	snaps map[string]*Snapshot
	dirs  map[string]string // id → fake dir path
}

func newMemStore() *memStore {
	return &memStore{snaps: map[string]*Snapshot{}, dirs: map[string]string{}}
}

func (s *memStore) Create(_ context.Context, snap *Snapshot) (string, error) {
	if snap == nil || snap.ID == "" {
		return "", errors.New("invalid snapshot")
	}
	dir := filepath.Join(os.TempDir(), "memstore", snap.ID)
	s.snaps[snap.ID] = snap
	s.dirs[snap.ID] = dir
	return dir, nil
}

func (s *memStore) Get(_ context.Context, id string) (*Snapshot, error) {
	snap, ok := s.snaps[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return snap, nil
}

func (s *memStore) List(_ context.Context, runID, target string) ([]*Snapshot, error) {
	var out []*Snapshot
	for _, snap := range s.snaps {
		if runID != "" && snap.RunID != runID {
			continue
		}
		if target != "" && snap.Target != target {
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memStore) Delete(_ context.Context, id string) error {
	delete(s.snaps, id)
	delete(s.dirs, id)
	return nil
}

func TestSnapshotManagerWithMemStore(t *testing.T) {
	store := newMemStore()
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	// With the in-memory store, CreateSnapshot will try to copy paths into
	// a non-existent dir (os.TempDir()/memstore/<id>). Use empty paths so
	// the snapshot record is created without copying.
	snap, err := mgr.CreateSnapshot(context.Background(), "run-1", "host-a",
		nil, SnapshotTypeFile, map[string]any{"k": "v"})
	require.NoError(t, err)
	assert.NotEmpty(t, snap.ID)

	got, err := store.Get(context.Background(), snap.ID)
	require.NoError(t, err)
	assert.Equal(t, "v", got.Metadata["k"])

	// List should find it.
	snaps, err := store.List(context.Background(), "run-1", "")
	require.NoError(t, err)
	assert.Len(t, snaps, 1)

	// Delete should remove it.
	require.NoError(t, store.Delete(context.Background(), snap.ID))
	snaps, err = store.List(context.Background(), "run-1", "")
	require.NoError(t, err)
	assert.Empty(t, snaps)
}

// --- concurrency -----------------------------------------------------------

func TestStoreConcurrentCreate(t *testing.T) {
	store := newTestStore(t)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)

	srcFile := filepath.Join(t.TempDir(), "f")
	writeFile(t, srcFile, "x")

	const n = 10
	ids := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			snap, err := mgr.CreateSnapshot(context.Background(), "run-concurrent", "host",
				[]string{srcFile}, SnapshotTypeFile, nil)
			if err != nil {
				t.Errorf("concurrent create: %v", err)
				return
			}
			ids <- snap.ID
		}()
	}

	collected := make([]string, 0, n)
	for i := 0; i < n; i++ {
		select {
		case id := <-ids:
			collected = append(collected, id)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent create")
		}
	}

	// All IDs should be unique.
	seen := map[string]bool{}
	for _, id := range collected {
		require.False(t, seen[id], "duplicate snapshot ID: %s", id)
		seen[id] = true
	}

	// All should be listed.
	snaps, err := store.List(context.Background(), "run-concurrent", "host")
	require.NoError(t, err)
	assert.Len(t, snaps, n)
}
