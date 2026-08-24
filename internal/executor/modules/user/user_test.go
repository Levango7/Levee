package user

import (
	"context"
	"errors"
	"io"
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
	mu            sync.Mutex
	execs         []string
	execResponses []execResponse
	execResult    *channel.ExecResult
	execErr       error
	uploadErr     error
	connected     bool
	uploads       []uploadRecord
}

type execResponse struct {
	result *channel.ExecResult
	err    error
}

// uploadRecord captures one Upload invocation for assertions.
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

	if len(m.execResponses) > 0 {
		r := m.execResponses[0]
		m.execResponses = m.execResponses[1:]
		return r.result, r.err
	}
	if m.execErr != nil {
		return nil, m.execErr
	}
	if m.execResult != nil {
		res := *m.execResult
		return &res, nil
	}
	return &channel.ExecResult{ExitCode: 0, Stdout: "", Stderr: ""}, nil
}

func (m *mockChannel) Upload(_ context.Context, path string, content io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.uploadErr != nil {
		return m.uploadErr
	}
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	m.uploads = append(m.uploads, uploadRecord{path: path, content: string(b)})
	return nil
}
func (m *mockChannel) Download(context.Context, string) (io.Reader, error) {
	return nil, errors.New("not implemented")
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

func (m *mockChannel) lastExec() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.execs) == 0 {
		return ""
	}
	return m.execs[len(m.execs)-1]
}

// --- module metadata ------------------------------------------------------

func TestModuleName(t *testing.T) {
	assert.Equal(t, "user", New().Name())
}

func TestModuleActions(t *testing.T) {
	assert.Equal(t, []string{"add", "remove", "modify"}, New().Actions())
}

func TestModuleIdempotent(t *testing.T) {
	assert.True(t, New().Idempotent())
}

// --- add ------------------------------------------------------------------

func TestAddCreatesUserWhenAbsent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // id -u -> absent
		{result: &channel.ExecResult{ExitCode: 0}}, // useradd
	}

	out, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args:    map[string]any{"name": "bob"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.execAt(1), "useradd 'bob'")
}

func TestAddWithAllAttributes(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // absent
		{result: &channel.ExecResult{ExitCode: 0}}, // useradd
	}

	_, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args: map[string]any{
			"name":   "bob",
			"uid":    2000,
			"shell":  "/bin/zsh",
			"home":   "/home/bob",
			"groups": "wheel,docker",
		},
		Channel: ch,
	})
	require.NoError(t, err)
	cmd := ch.execAt(1)
	assert.Contains(t, cmd, "useradd")
	assert.Contains(t, cmd, "-u '2000'")
	assert.Contains(t, cmd, "-s '/bin/zsh'")
	assert.Contains(t, cmd, "-d '/home/bob'")
	assert.Contains(t, cmd, "-G 'wheel,docker'")
	assert.Contains(t, cmd, "bob")
}

func TestAddSkipsWhenPresent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "1000"}}, // id -u -> present
	}

	out, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args:    map[string]any{"name": "bob"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed, "already present -> no change")
	assert.Equal(t, 1, ch.execCount(), "only the id -u check should have run")
}

func TestAddWithPassword(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // absent
		{result: &channel.ExecResult{ExitCode: 0}}, // useradd
		{result: &channel.ExecResult{ExitCode: 0}}, // chpasswd
	}

	_, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args:    map[string]any{"name": "bob", "password": "s3cret"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(2), "chpasswd")
	// The credential travels via the uploaded temp file, never in argv:
	// the command references the file and cleans it up, but must not
	// contain the plaintext password.
	assert.Contains(t, ch.execAt(2), "rm -f")
	assert.NotContains(t, ch.execAt(2), "s3cret",
		"plaintext password must not appear in any Exec command string")

	// Upload received name:password content and chpasswd reads that file.
	require.Len(t, ch.uploads, 1, "exactly one credential upload expected")
	assert.Equal(t, "bob:s3cret\n", ch.uploads[0].content)
	assert.True(t, strings.HasPrefix(ch.uploads[0].path, "/dev/shm/.levee-chpasswd-"),
		"upload target must live on tmpfs (/dev/shm), got %q", ch.uploads[0].path)
	assert.Contains(t, ch.execAt(2), ch.uploads[0].path,
		"chpasswd must read from the uploaded temp file")

	// The consuming Exec hardens the exposure window: restrictive umask
	// before reading the file, cleanup before exit.
	cmd := ch.execAt(2)
	assert.True(t, strings.HasPrefix(cmd, "umask 077; "),
		"chpasswd Exec must start with `umask 077` to restrict file modes, got %q", cmd)
	assert.Regexp(t, `rm -f '[^']+'; exit \$rc$`, cmd,
		"the temp file must be removed before propagating chpasswd's exit code")
}

// failingUploadChannel fails Upload for /dev/shm targets so the /tmp
// fallback path can be exercised.
type failingUploadChannel struct {
	mockChannel
}

func (m *failingUploadChannel) Upload(ctx context.Context, path string, content io.Reader) error {
	if strings.HasPrefix(path, "/dev/shm/") {
		return errors.New("no space left on device")
	}
	return m.mockChannel.Upload(ctx, path, content)
}

func TestSetPassword_FallsBackToTmpWhenShmUploadFails(t *testing.T) {
	ch := &failingUploadChannel{mockChannel: mockChannel{connected: true}}

	r, err := setPassword(context.Background(), ch, "bob", "s3cret")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Equal(t, 0, r.exit)

	// Two uploads attempted: first to /dev/shm (failed), then /tmp.
	require.Len(t, ch.uploads, 1, "only the successful fallback upload is recorded")
	assert.True(t, strings.HasPrefix(ch.uploads[0].path, "/tmp/.levee-chpasswd-"),
		"fallback target must be under /tmp, got %q", ch.uploads[0].path)

	// Best-effort cleanup of the failed shm target must have been sent.
	require.GreaterOrEqual(t, ch.execCount(), 1)
	assert.Contains(t, ch.execAt(0), "rm -f '/dev/shm/.levee-chpasswd-")

	// The consuming Exec reads the /tmp fallback with umask + cleanup.
	last := ch.lastExec()
	assert.True(t, strings.HasPrefix(last, "umask 077; "))
	assert.Contains(t, last, ch.uploads[0].path)
	assert.NotContains(t, last, "s3cret")
}

func TestSetPassword_UploadFailsEverywhereReturnsErrorAfterCleanup(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.uploadErr = errors.New("sftp unavailable")

	_, err := setPassword(context.Background(), ch, "bob", "s3cret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload credentials")

	// Cleanup was still attempted for both candidate paths.
	found := false
	for _, cmd := range ch.execs {
		if strings.HasPrefix(cmd, "rm -f ") && strings.Contains(cmd, ".levee-chpasswd-") {
			found = true
		}
	}
	assert.True(t, found, "a best-effort rm -f must be issued after upload failure")
}

func TestAddWithSSHKey(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // absent
		{result: &channel.ExecResult{ExitCode: 0}}, // useradd
		{result: &channel.ExecResult{ExitCode: 0}}, // ssh key snippet
	}

	out, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args:    map[string]any{"name": "bob", "ssh_key": "ssh-rsa AAAA... bob@host"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	// The third call should be the ssh key install snippet.
	snippet := ch.execAt(2)
	assert.Contains(t, snippet, "authorized_keys")
	assert.Contains(t, snippet, "ssh-rsa AAAA... bob@host")
}

func TestAddMissingName(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "add", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
	assert.Contains(t, err.Error(), "name")
}

// --- remove ---------------------------------------------------------------

func TestRemoveWhenPresent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0, Stdout: "1000"}}, // present
		{result: &channel.ExecResult{ExitCode: 0}},                 // userdel -r
	}

	out, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{"name": "bob"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.True(t, out.Changed)
	assert.Contains(t, ch.lastExec(), "userdel -r 'bob'")
}

func TestRemoveWhenAbsent(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 1}}, // absent
	}

	out, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{"name": "bob"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.False(t, out.Changed)
	assert.Equal(t, 1, ch.execCount())
}

func TestRemoveMissingName(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "remove", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
}

// --- modify ---------------------------------------------------------------

func TestModifyRunsUsermod(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0}}, // usermod
	}

	_, err := New().Execute(context.Background(), "modify", executor.ModuleInput{
		Args:    map[string]any{"name": "bob", "shell": "/bin/bash", "group": "staff"},
		Channel: ch,
	})
	require.NoError(t, err)
	cmd := ch.lastExec()
	assert.Contains(t, cmd, "usermod")
	assert.Contains(t, cmd, "-s '/bin/bash'")
	assert.Contains(t, cmd, "-g 'staff'")
	assert.Contains(t, cmd, "bob")
}

func TestModifyWithSSHKey(t *testing.T) {
	ch := &mockChannel{connected: true}
	ch.execResponses = []execResponse{
		{result: &channel.ExecResult{ExitCode: 0}}, // usermod
		{result: &channel.ExecResult{ExitCode: 0}}, // ssh key
	}

	_, err := New().Execute(context.Background(), "modify", executor.ModuleInput{
		Args:    map[string]any{"name": "bob", "ssh_key": "ssh-ed25519 AAAA... bob@host"},
		Channel: ch,
	})
	require.NoError(t, err)
	assert.Contains(t, ch.execAt(1), "authorized_keys")
	assert.Contains(t, ch.execAt(1), "ssh-ed25519 AAAA... bob@host")
}

func TestModifyMissingName(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "modify", executor.ModuleInput{
		Args:    map[string]any{},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing argument")
}

// --- dispatch / unknown action --------------------------------------------

func TestUnknownAction(t *testing.T) {
	ch := &mockChannel{connected: true}
	_, err := New().Execute(context.Background(), "bogus", executor.ModuleInput{
		Args:    map[string]any{"name": "bob"},
		Channel: ch,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported action")
}

// --- registration ---------------------------------------------------------

func TestModuleRegistered(t *testing.T) {
	m, ok := executor.DefaultExecutor().Module("user")
	require.True(t, ok, "user module should self-register via init()")
	assert.Equal(t, "user", m.Name())
}

// --- helpers --------------------------------------------------------------

func TestBuildUseraddCmd(t *testing.T) {
	cmd := buildUseraddCmd(map[string]any{"name": "bob"})
	assert.Equal(t, "useradd 'bob'", cmd)
}

func TestBuildUseraddCmdFull(t *testing.T) {
	cmd := buildUseraddCmd(map[string]any{
		"name":   "bob",
		"uid":    2000,
		"shell":  "/bin/zsh",
		"home":   "/home/bob",
		"groups": "wheel",
	})
	assert.Contains(t, cmd, "-u '2000'")
	assert.Contains(t, cmd, "-s '/bin/zsh'")
	assert.Contains(t, cmd, "-d '/home/bob'")
	assert.Contains(t, cmd, "-G 'wheel'")
}

func TestBuildUsermodCmd(t *testing.T) {
	cmd := buildUsermodCmd(map[string]any{"name": "bob", "shell": "/bin/sh"})
	assert.Equal(t, "usermod -s '/bin/sh' 'bob'", cmd)
}

// TestBuildUserCmdsQuoteInjection pins the injection fix: workflow-supplied
// attribute values must never break out of their shell arguments.
func TestBuildUserCmdsQuoteInjection(t *testing.T) {
	malicious := "/tmp/x; touch /tmp/pwned"
	cmd := buildUseraddCmd(map[string]any{"name": "bob", "home": malicious})
	assert.Contains(t, cmd, `-d '/tmp/x; touch /tmp/pwned'`)
	assert.NotContains(t, cmd, "home /tmp/x;")

	cmd = buildUsermodCmd(map[string]any{"name": "bob", "groups": "a; reboot"})
	assert.Contains(t, cmd, `-G 'a; reboot'`)

	// A single quote in the value is escaped with the '\'' idiom.
	cmd = buildUseraddCmd(map[string]any{"name": "b'ob"})
	assert.Contains(t, cmd, `'b'\''ob'`)
}

func TestToIntString(t *testing.T) {
	assert.Equal(t, "2000", toIntString(2000))
	assert.Equal(t, "2000", toIntString(int64(2000)))
	assert.Equal(t, "2000", toIntString(float64(2000)))
	assert.Equal(t, "2000", toIntString("2000"))
	assert.Equal(t, "", toIntString(nil))
	assert.Equal(t, "", toIntString([]int{1}))
}

func TestShellQuote(t *testing.T) {
	assert.Equal(t, `'simple'`, shellQuote("simple"))
	// Single quote inside is escaped with the '\'' idiom.
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
}

func TestStringOk(t *testing.T) {
	args := map[string]any{"a": "x", "b": 1}
	v, ok := stringOk(args, "a")
	assert.True(t, ok)
	assert.Equal(t, "x", v)
	_, ok = stringOk(args, "b")
	assert.False(t, ok)
	_, ok = stringOk(args, "missing")
	assert.False(t, ok)
}

// keep strings import honest.
var _ = strings.TrimSpace
