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
	assert.Contains(t, ch.execAt(2), ch.uploads[0].path,
		"chpasswd must read from the uploaded temp file")
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
