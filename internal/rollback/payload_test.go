package rollback

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadRoundTrip(t *testing.T) {
	dir := t.TempDir()

	payloads := map[string]string{
		"/etc/nginx/nginx.conf": "worker_processes 2;\n",
		"/etc/hosts":            "127.0.0.1 localhost\n",
	}
	require.NoError(t, WriteSnapshotPayloads(dir, payloads))

	// The on-disk layout must match the manager's local-FS copyPaths:
	// files/<flattened> plus a paths.json mapping back to originals.
	pathMapFile := filepath.Join(dir, "paths.json")
	require.FileExists(t, pathMapFile)

	got, err := ReadSnapshotPayloads(dir)
	require.NoError(t, err)
	assert.Equal(t, payloads, got)
}

func TestPayloadEmptyDirIsNoPayload(t *testing.T) {
	dir := t.TempDir()
	// Write an empty payload set (audit-only snapshot): paths.json exists,
	// files/ empty, read yields an empty map — restore no-op semantics.
	require.NoError(t, WriteSnapshotPayloads(dir, nil))
	got, err := ReadSnapshotPayloads(dir)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPayloadMissingDirIsEmpty(t *testing.T) {
	// A snapshot directory with no files/ (never captured, or created by
	// an older path) reads as empty rather than erroring — matching the
	// manager's RestoreSnapshot no-op tolerance.
	got, err := ReadSnapshotPayloads(filepath.Join(t.TempDir(), "nonexistent"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPayloadEmptyDirRejected(t *testing.T) {
	err := WriteSnapshotPayloads("", map[string]string{"/a": "b"})
	require.Error(t, err)

	_, err = ReadSnapshotPayloads("")
	require.Error(t, err)
}

func TestPayloadSkipsEmptyOrigPath(t *testing.T) {
	dir := t.TempDir()
	// An empty original path is silently dropped (parser never produces
	// one; defensive).
	require.NoError(t, WriteSnapshotPayloads(dir, map[string]string{"": "x", "/ok": "y"}))
	got, err := ReadSnapshotPayloads(dir)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"/ok": "y"}, got)
}

func TestPayloadLayoutMatchesManagerCapture(t *testing.T) {
	// The remote-capture layout and the manager's local copyPaths layout
	// must be byte-compatible: a snapshot written via payloads must be
	// restorable by the manager's own RestoreSnapshot. Prove it with a
	// real file: copy a local file through the manager (local path) and
	// through WriteSnapshotPayloads, then compare directory contents.
	src := filepath.Join(t.TempDir(), "app.conf")
	require.NoError(t, os.WriteFile(src, []byte("listen 80\n"), 0o600))

	mgrDir := t.TempDir()
	store, err := NewFileSnapshotStore(mgrDir)
	require.NoError(t, err)
	mgr, err := NewSnapshotManager(store)
	require.NoError(t, err)
	localSnap, err := mgr.CreateSnapshot(t.Context(), "run-cmp", "host-a", []string{src}, SnapshotTypeFile, nil)
	require.NoError(t, err)

	payloadDir := t.TempDir()
	require.NoError(t, WriteSnapshotPayloads(payloadDir, map[string]string{src: "listen 80\n"}))

	// Same flattened file name in files/ for both paths.
	localFiles, err := os.ReadDir(filepath.Join(localSnap.Path, "files"))
	require.NoError(t, err)
	payloadFiles, err := os.ReadDir(filepath.Join(payloadDir, "files"))
	require.NoError(t, err)
	require.Len(t, localFiles, 1)
	require.Len(t, payloadFiles, 1)
	assert.Equal(t, localFiles[0].Name(), payloadFiles[0].Name())
}
