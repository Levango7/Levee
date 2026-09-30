// snapshotter_test.go exercises the channel-aware snapshotter end to end
// against a scripted fake transport: base64 capture commands are decoded
// from a virtual target filesystem, restore Uploads are recorded back into
// it, and the change-id keying is proven across the automatic closure
// rollback and the manual RollbackChange path (which passes a different
// execution run id).

package wiring

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/rollback"
)

// --- scripted snapshot transport -------------------------------------------

// snapVFS is the fake target machine's filesystem plus a command/upload
// log.
type snapVFS struct {
	mu       sync.Mutex
	files    map[string]string // path -> content
	cmds     []string          // every Exec command seen
	uploads  map[string]string // path -> uploaded content
	failPath string            // when set, Exec on this path fails
}

func newSnapVFS(files map[string]string) *snapVFS {
	if files == nil {
		files = map[string]string{}
	}
	return &snapVFS{files: files, uploads: map[string]string{}}
}

func (v *snapVFS) exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.cmds = append(v.cmds, cmd)

	// The snapshotter issues exactly one command shape: base64 '<path>'.
	const prefix = "base64 '"
	if !strings.HasPrefix(cmd, prefix) || !strings.HasSuffix(cmd, "'") {
		return &channel.ExecResult{ExitCode: 127, Stderr: "snapVFS: unsupported command: " + cmd}, nil
	}
	path := cmd[len(prefix) : len(cmd)-1]
	if v.failPath == path {
		return &channel.ExecResult{ExitCode: 1, Stderr: "snapVFS: forced failure"}, nil
	}
	content, ok := v.files[path]
	if !ok {
		return &channel.ExecResult{ExitCode: 1, Stderr: fmt.Sprintf("snapVFS: %s: no such file", path)}, nil
	}
	return &channel.ExecResult{
		ExitCode: 0,
		Stdout:   base64.StdEncoding.EncodeToString([]byte(content)),
	}, nil
}

func (v *snapVFS) upload(ctx context.Context, remotePath string, content io.Reader) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.uploads[remotePath] = string(data)
	return nil
}

// snapChannel binds the VFS to one host.
type snapChannel struct {
	host string
	vfs  *snapVFS
}

func (c *snapChannel) Connect(context.Context) error { return nil }
func (c *snapChannel) Close() error                  { return nil }
func (c *snapChannel) IsConnected() bool             { return true }
func (c *snapChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	return c.vfs.exec(ctx, cmd)
}
func (c *snapChannel) Upload(ctx context.Context, p string, r io.Reader) error {
	return c.vfs.upload(ctx, p, r)
}
func (c *snapChannel) Download(ctx context.Context, p string) (io.Reader, error) {
	return nil, fmt.Errorf("snapVFS: Download not supported")
}

type snapFactory struct{ vfs *snapVFS }

func (f snapFactory) Create(target channel.Target) (channel.Channel, error) {
	return &snapChannel{host: target.Host(), vfs: f.vfs}, nil
}

// --- helpers ------------------------------------------------------------------

// newSnapEngine assembles an Engine + inventory over the scripted VFS and
// returns a ready runExec bound to the change id.
func newSnapEngine(t *testing.T, vfs *snapVFS, host, changeID string, snapshotDir string) (*runExec, *Engine) {
	t.Helper()
	reg := channel.NewChannelRegistry()
	reg.Register("local", snapFactory{vfs: vfs})

	opts := []Option{WithChannelRegistry(reg)}
	if snapshotDir != "" {
		opts = append(opts, WithSnapshotDir(snapshotDir))
	}
	e, store := newLoopEngine(t, &loopRecorder{}, opts...)
	seedLocalTargets(t, store, host)

	rx, err := newRunExec(context.Background(), e, nil)
	require.NoError(t, err)
	t.Cleanup(rx.close)
	return rx, e
}

// snapStep builds a plan step declaring snapshot rollback over paths.
func snapStep(name string, paths ...string) plan.PlanStep {
	return plan.PlanStep{
		Name:   name,
		Module: "file",
		Action: "copy",
		Rollback: &dsl.RollbackSpec{
			Strategy:      "snapshot",
			SnapshotPaths: paths,
		},
	}
}

// --- tests ---------------------------------------------------------------------

func TestSnapshotterCaptureStoresRemoteFiles(t *testing.T) {
	vfs := newSnapVFS(map[string]string{
		"/etc/app.conf": "listen 80\n",
		"/etc/app.key":  "secret",
	})
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-1", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-1")

	step := snapStep("push-conf", "/etc/app.conf", "/etc/app.key")

	require.NoError(t, s.CaptureForStep(context.Background(), "run-1", "web1", step))

	// Both files were fetched with the exact base64 command shape.
	cmds := vfs.commands()
	assert.Len(t, cmds, 2)
	assert.Contains(t, cmds, "base64 '/etc/app.conf'")
	assert.Contains(t, cmds, "base64 '/etc/app.key'")

	// The store recorded the snapshot keyed by the CHANGE id.
	snaps, err := store.List(context.Background(), "chg-1", "web1")
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	assert.Equal(t, "push-conf", snaps[0].Metadata["step"])

	// Payloads land in the store directory, byte-identical.
	got, err := rollback.ReadSnapshotPayloads(snaps[0].Path)
	require.NoError(t, err)
	assert.Equal(t, "listen 80\n", got["/etc/app.conf"])
	assert.Equal(t, "secret", got["/etc/app.key"])
}

func TestSnapshotterRestoreUploadsBack(t *testing.T) {
	vfs := newSnapVFS(map[string]string{"/etc/app.conf": "ORIGINAL"})
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-2", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-2")

	step := snapStep("push-conf", "/etc/app.conf")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-1", "web1", step))

	// "Apply" mutates the target, then restore must push ORIGINAL back.
	vfs.setFile("/etc/app.conf", "MUTATED")
	require.NoError(t, s.RestoreForStep(context.Background(), "run-999", "web1", step))
	assert.Equal(t, "ORIGINAL", vfs.uploaded("/etc/app.conf"))
}

func TestSnapshotterChangeKeySurvivesDifferentRunIDs(t *testing.T) {
	// The manual RollbackChange path knows only the change id; its
	// execution run id differs from the capture run id. Restore keyed by
	// the change must still find the capture.
	vfs := newSnapVFS(map[string]string{"/etc/app.conf": "BEFORE"})
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-3", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-3")

	step := snapStep("push-conf", "/etc/app.conf")
	require.NoError(t, s.CaptureForStep(context.Background(), "run-capture", "web1", step))

	// A DIFFERENT run id restores fine — the key is the change id.
	vfs.setFile("/etc/app.conf", "MUTATED")
	require.NoError(t, s.RestoreForStep(context.Background(), "run-restore", "web1", step))
	assert.Equal(t, "BEFORE", vfs.uploaded("/etc/app.conf"))
}

func TestSnapshotterRestoreNoSnapshotFails(t *testing.T) {
	// No capture ever ran: restore fails closed with a diagnosable error,
	// never a silent skip (an unrestorable snapshot step must be visible).
	vfs := newSnapVFS(nil)
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-4", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-4")

	step := snapStep("push-conf", "/etc/app.conf")

	err = s.RestoreForStep(context.Background(), "run-x", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no snapshot recorded")
}

func TestSnapshotterCaptureFailureFailsClosed(t *testing.T) {
	// The target file does not exist: capture errors (exit 1 from the
	// fake transport), surfacing the path — the closure aborts before
	// any mutation (design: 快照创建失败则该目标机不进 apply).
	vfs := newSnapVFS(nil) // /etc/app.conf absent
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-5", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-5")

	step := snapStep("push-conf", "/etc/app.conf")

	err = s.CaptureForStep(context.Background(), "run-1", "web1", step)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/etc/app.conf")
}

func TestSnapshotterAuditOnlyCapture(t *testing.T) {
	// strategy: snapshot with NO declared paths: an audit-only record is
	// still written (empty payloads), restore is a documented no-op.
	vfs := newSnapVFS(nil)
	dir := t.TempDir()
	rx, _ := newSnapEngine(t, vfs, "web1", "chg-6", dir)

	store, err := rollback.NewFileSnapshotStore(dir)
	require.NoError(t, err)
	mgr, err := rollback.NewSnapshotManager(store)
	require.NoError(t, err)
	s := newRemoteSnapshotter(rx, mgr, "chg-6")

	step := snapStep("restart") // strategy snapshot, no paths

	require.NoError(t, s.CaptureForStep(context.Background(), "run-1", "web1", step))
	// No transport command was issued (nothing to fetch).
	assert.Empty(t, vfs.commands())

	require.NoError(t, s.RestoreForStep(context.Background(), "run-2", "web1", step))
	assert.Empty(t, vfs.uploads)
}

func TestShellQuoteSingle(t *testing.T) {
	assert.Equal(t, "'/etc/app.conf'", shellQuoteSingle("/etc/app.conf"))
	assert.Equal(t, "'it'\\''s'", shellQuoteSingle("it's"))
}

// --- snapVFS convenience accessors (locked) ------------------------------------

func (v *snapVFS) commands() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.cmds...)
}

func (v *snapVFS) uploaded(path string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.uploads[path]
}

func (v *snapVFS) setFile(path, content string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.files[path] = content
}
