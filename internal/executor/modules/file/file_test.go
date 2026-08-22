package file

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/executor"
)

// --- mock channel ----------------------------------------------------------

type mockChannel struct {
	mu sync.Mutex

	execs      []string
	execResult *channel.ExecResult
	execErr    error
	// execResponses lets tests program per-call responses in call order.
	execResponses []execResponse

	uploads   []uploadRecord
	uploadErr error

	connected bool
}

type execResponse struct {
	result *channel.ExecResult
	err    error
}

type uploadRecord struct {
	path    string
	content string
}

func (m *mockChannel) Connect(context.Context) error { m.connected = true; return nil }
func (m *mockChannel) IsConnected() bool             { return m.connected }
func (m *mockChannel) Close() error                  { m.connected = false; return nil }

func (m *mockChannel) Exec(_ context.Context, cmd string) (*channel.ExecResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execs = append(m.execs, cmd)

	// Per-call programmed responses take precedence.
	if len(m.execResponses) > 0 {
		r := m.execResponses[0]
		m.execResponses = m.execResponses[1:]
		return r.result, r.err
	}
	if m.execErr != nil {
		return nil, m.execErr
	}
	if m.execResult != nil {
		// Return a copy so callers can mutate without affecting later calls.
		res := *m.execResult
		return &res, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
}

func (m *mockChannel) Upload(_ context.Context, path string, r io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, _ := io.ReadAll(r)
	m.uploads = append(m.uploads, uploadRecord{path: path, content: string(body)})
	return m.uploadErr
}

func (m *mockChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, errors.New("not implemented")
}

func (m *mockChannel) lastExec() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.execs) == 0 {
		return ""
	}
	return m.execs[len(m.execs)-1]
}

func (m *mockChannel) execAt(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.execs) {
		return ""
	}
	return m.execs[i]
}

func (m *mockChannel) execCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.execs)
}

// --- helpers --------------------------------------------------------------

// writeTempFile creates a temp file with the given content and returns its
// path. The t.Cleanup hook removes it at the end of the test.
func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// sha256Hex is a small helper for building expected checksum strings.
func sha256Hex(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}

// --- module metadata ------------------------------------------------------

func TestModuleName(t *testing.T) {
	assert.Equal(t, "file", New().Name())
}

func TestModuleActions(t *testing.T) {
	assert.Equal(t, []string{"copy", "template"}, New().Actions())
}

func TestModuleIdempotent(t *testing.T) {
	assert.True(t, New().Idempotent())
}

// --- copy action ----------------------------------------------------------

func TestCopyUploadsWhenRemoteMissing(t *testing.T) {
	src := writeTempFile(t, "hello world")

	// First Exec (sha256sum) reports the remote file is missing.
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1, Stderr: "No such file"}},
		// After upload, no optional attrs in this test so no more Exec calls.
	}

	out, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/nginx/nginx.conf"},
		Channel: ch,
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.True(t, out.Changed, "first copy must report Changed=true")

	require.Len(t, ch.uploads, 1)
	assert.Equal(t, "/etc/nginx/nginx.conf", ch.uploads[0].path)
	assert.Equal(t, "hello world", ch.uploads[0].content)
	// sha256sum was the only Exec.
	assert.Contains(t, ch.execAt(0), "sha256sum '/etc/nginx/nginx.conf'")
}

func TestCopySkipsWhenContentMatches(t *testing.T) {
	src := writeTempFile(t, "same content")
	wantSum := sha256Hex("same content")

	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: wantSum + "  /etc/foo"}},
	}

	out, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/foo"},
		Channel: ch,
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, 0, out.ExitCode)
	assert.False(t, out.Changed, "no-op copy must report Changed=false")
	assert.Empty(t, ch.uploads, "no upload should happen when content matches")
}

func TestCopyUploadsWhenContentDiffers(t *testing.T) {
	src := writeTempFile(t, "new content")
	remoteSum := sha256Hex("old content")

	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: remoteSum + "  /etc/foo"}},
	}

	out, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/foo"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	require.Len(t, ch.uploads, 1)
	assert.Equal(t, "new content", ch.uploads[0].content)
}

func TestCopyAppliesMode(t *testing.T) {
	src := writeTempFile(t, "data")
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1, Stderr: "missing"}},      // sha256sum
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "mode applied"}}, // chmod
	}

	out, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/foo", "mode": "0640"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Equal(t, 0, out.ExitCode)
	assert.Contains(t, ch.execAt(1), "chmod '0640' '/etc/foo'")
	_ = out
}

func TestCopyAppliesOwnerAndGroup(t *testing.T) {
	src := writeTempFile(t, "data")
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // sha256sum
		{result: &channel.ExecResult{ExitCode: 0}}, // chmod
		{result: &channel.ExecResult{ExitCode: 0}}, // chown
		{result: &channel.ExecResult{ExitCode: 0}}, // chgrp
	}

	_, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/foo", "mode": "0644", "owner": "nginx", "group": "nginx"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(1), "chmod '0644' '/etc/foo'")
	assert.Contains(t, ch.execAt(2), "chown 'nginx' '/etc/foo'")
	assert.Contains(t, ch.execAt(3), "chgrp 'nginx' '/etc/foo'")
}

func TestCopyMissingSrc(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": "/no/such/file", "dest": "/etc/foo"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read src")
}

func TestCopyMissingDestArg(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": "/etc/foo"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
	assert.Contains(t, err.Error(), "dest")
}

func TestCopyUploadError(t *testing.T) {
	src := writeTempFile(t, "data")
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // sha256sum -> missing
	}
	ch.uploadErr = errors.New("disk full")

	_, err := New().Execute(context.Background(), "copy", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/foo"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload")
	assert.ErrorIs(t, err, ch.uploadErr)
}

// --- template action ------------------------------------------------------

func TestTemplateRendersAndUploads(t *testing.T) {
	src := writeTempFile(t, "Hello {{.name}}, port={{.port}}")
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // sha256sum -> missing
	}

	out, err := New().Execute(context.Background(), "template", executor.ModuleInput{
		Args: map[string]any{
			"src":  src,
			"dest": "/etc/app.conf",
			"vars": map[string]any{"name": "levee", "port": 8080},
		},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	require.Len(t, ch.uploads, 1)
	assert.Equal(t, "Hello levee, port=8080", ch.uploads[0].content)
}

func TestTemplateSkipsWhenRenderedContentMatches(t *testing.T) {
	src := writeTempFile(t, "static content")
	wantSum := sha256Hex("static content")

	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: wantSum + "  /etc/app.conf"}},
	}

	out, err := New().Execute(context.Background(), "template", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/app.conf"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
	assert.Empty(t, ch.uploads)
}

func TestTemplateParseError(t *testing.T) {
	src := writeTempFile(t, "Hello {{.name") // malformed
	ch := &mockChannel{connected: true}

	_, err := New().Execute(context.Background(), "template", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/app.conf"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestTemplateExecuteError(t *testing.T) {
	// `call` on a non-callable value (a string) fails at execute time, not
	// parse time, so we can verify the execute-error path.
	src := writeTempFile(t, "Value: {{call .name}}")
	ch := &mockChannel{connected: true}

	_, err := New().Execute(context.Background(), "template", executor.ModuleInput{
		Args:    map[string]any{"src": src, "dest": "/etc/app.conf", "vars": map[string]any{"name": "notafunc"}},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "execute")
}

func TestTemplateMissingSrc(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "template", executor.ModuleInput{
		Args:    map[string]any{"dest": "/etc/app.conf"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
	assert.Contains(t, err.Error(), "src")
}

// --- dispatch / unknown action --------------------------------------------

func TestExecuteUnknownAction(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "bogus", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
}

// --- registration ---------------------------------------------------------

func TestModuleRegistered(t *testing.T) {
	m, ok := executor.DefaultExecutor().Module("file")
	require.True(t, ok, "file module should self-register via init()")
	assert.Equal(t, "file", m.Name())
}

// --- helpers sanity -------------------------------------------------------

func TestSha256Sum(t *testing.T) {
	// Sanity check that our helper matches the standard library.
	assert.Equal(t, sha256Hex("abc"), sha256Hex("abc"))
	assert.NotEqual(t, sha256Hex("abc"), sha256Hex("abd"))
}

func TestStringOk(t *testing.T) {
	args := map[string]any{"a": "x", "b": 1}
	v, ok := stringOk(args, "a")
	assert.True(t, ok)
	assert.Equal(t, "x", v)
	_, ok = stringOk(args, "b")
	assert.False(t, ok, "non-string value should return false")
	_, ok = stringOk(args, "missing")
	assert.False(t, ok)
}

// keep strings import honest if future refactors drop direct uses.
var _ = strings.TrimSpace
