package local

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
)

// resetPolicy restores the process-wide fail-closed defaults between
// tests. Every test that installs a policy must defer this.
func resetPolicy() {
	policyMu.Lock()
	policy = Policy{RootDir: "", Programs: map[string]ArgPolicy{}}
	enabled = false
	policyMu.Unlock()
}

// mustPolicy installs a permissive-for-test sandbox policy rooted at dir
// with the given program allow-list.
func mustPolicy(t *testing.T, dir string, programs map[string]ArgPolicy) {
	t.Helper()
	SetPolicy(Policy{RootDir: dir, Programs: programs})
	t.Cleanup(resetPolicy)
}

// --- metadata ----------------------------------------------------------------

func TestTarget(t *testing.T) {
	tgt := Target{Hostname: "localhost"}
	assert.Equal(t, "localhost", tgt.Host())
	assert.Equal(t, 0, tgt.Port())
	assert.Equal(t, "local", tgt.Type())
	assert.Equal(t, channel.CredentialRef{}, tgt.Credentials())
}

func TestFactoryTypeGuard(t *testing.T) {
	f := NewFactory()
	ch, err := f.Create(Target{Hostname: "h"})
	require.NoError(t, err)
	require.NotNil(t, ch)
	require.NoError(t, ch.Close())

	// A foreign target type must be rejected, not silently mis-executed.
	bogus := targetWith{typ: "ssh"}
	_, err = f.Create(bogus)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"ssh"`)
}

type targetWith struct{ typ string }

func (t targetWith) Host() string                       { return "h" }
func (t targetWith) Port() int                          { return 0 }
func (t targetWith) Type() string                       { return t.typ }
func (t targetWith) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// --- fail-closed defaults ----------------------------------------------------

func TestDisabledByDefault(t *testing.T) {
	resetPolicy()
	defer resetPolicy()

	ch := New()
	err := ch.Connect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrDisabled)

	// Exec is denied even without Connect.
	_, err = ch.Exec(context.Background(), "echo hi")
	assert.ErrorIs(t, err, ErrDisabled)
}

func TestEmptyPolicyDeniesEverything(t *testing.T) {
	// Enabled but zero Programs map: every program is denied.
	resetPolicy()
	defer resetPolicy()
	SetPolicy(Policy{RootDir: t.TempDir()})

	_, err := New().Exec(context.Background(), "echo hi")
	require.Error(t, err)
	if runtime.GOOS != "windows" {
		// On unix the denial comes from the empty allow-list; on
		// windows the platform gate fires first (also a denial).
		assert.ErrorIs(t, err, ErrPolicyDenied)
	}
}

// --- policy gate --------------------------------------------------------------

func TestCheckPolicyProgramAllowList(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{
		"echo":  ArgShell,
		"mysql": ArgNone,
	})
	ctx := context.Background()

	// Allowed program.
	require.NoError(t, CheckPolicy(ctx, "echo", []string{"hello"}))

	// Not on the list.
	err := CheckPolicy(ctx, "rm", []string{"-rf", "/"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
	assert.Contains(t, err.Error(), `"rm"`)
}

func TestCheckPolicyArgNone(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"mysql": ArgNone})
	ctx := context.Background()

	require.NoError(t, CheckPolicy(ctx, "mysql", nil))
	err := CheckPolicy(ctx, "mysql", []string{"-u", "root"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

func TestCheckPolicyArgPrefix(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"pt-online-schema-change": ArgPrefix})
	ctx := context.Background()

	require.NoError(t, CheckPolicy(ctx, "pt-online-schema-change", []string{"--execute", "--alter"}))
	err := CheckPolicy(ctx, "pt-online-schema-change", []string{"--execute", "; rm -rf /"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

func TestCheckPolicyContextCancellation(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CheckPolicy(ctx, "echo", nil)
	require.Error(t, err)
}

// --- exec ---------------------------------------------------------------------

func TestExecRunsAllowedCommand(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})
	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	if runtime.GOOS == "windows" {
		// Platform gate: no command runs on windows.
		_, err := ch.Exec(context.Background(), "echo levee-local-test")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported on windows")
		return
	}

	res, err := ch.Exec(context.Background(), "echo levee-local-test")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Contains(t, res.Stdout, "levee-local-test")
	assert.NotZero(t, res.Duration)
}

func TestExecCapturesFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platform gate covered by TestExecRunsAllowedCommand")
	}
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"false": ArgShell})

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	res, err := ch.Exec(context.Background(), "false")
	require.NoError(t, err) // process ran; failure is a result, not an error
	assert.NotEqual(t, 0, res.ExitCode)
}

func TestExecDeniesUnlistedProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platform gate covered by TestExecRunsAllowedCommand")
	}
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	_, err := ch.Exec(context.Background(), "cat /etc/passwd")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

func TestExecEmptyCommand(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})
	_, err := New().Exec(context.Background(), "   ")
	require.Error(t, err)
}

func TestExecUnknownProgramFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platform gate covered by TestExecRunsAllowedCommand")
	}
	// Allow-list entry for a program that does not exist: the policy
	// allows it, LookPath rejects it — surfaced as a typed error, not a
	// silent exit 0.
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"definitely-not-a-real-program": ArgShell})
	_, err := New().Exec(context.Background(), "definitely-not-a-real-program")
	require.Error(t, err)
}

// --- upload / download ----------------------------------------------------------

func TestUploadDownloadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mustPolicy(t, dir, nil)

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	content := "hello sandbox"
	require.NoError(t, ch.Upload(context.Background(), "notes/file.txt", strings.NewReader(content)))

	r, err := ch.Download(context.Background(), "notes/file.txt")
	require.NoError(t, err)
	defer func() { _ = r.(io.Closer).Close() }()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, content, string(got))
}

func TestUploadRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	mustPolicy(t, dir, nil)

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	// Lexical traversal.
	err := ch.Upload(context.Background(), "../escape.txt", strings.NewReader("x"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)

	// Absolute path outside the root.
	err = ch.Upload(context.Background(), filepath.Join(os.TempDir(), "escape.txt"), strings.NewReader("x"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

func TestDownloadRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	mustPolicy(t, dir, nil)

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	_, err := ch.Download(context.Background(), "../../etc/passwd")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

func TestUploadRequiresRootDir(t *testing.T) {
	// Enabled via SetPolicy but RootDir empty: sandbox paths fail closed.
	resetPolicy()
	defer resetPolicy()
	SetPolicy(Policy{Programs: map[string]ArgPolicy{"echo": ArgShell}})

	err := New().Upload(context.Background(), "x.txt", strings.NewReader("x"))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPolicyDenied)
}

// --- lifecycle -------------------------------------------------------------------

func TestConnectIdempotent(t *testing.T) {
	mustPolicy(t, t.TempDir(), nil)
	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	require.NoError(t, ch.Connect(context.Background()))
	assert.True(t, ch.IsConnected())
	require.NoError(t, ch.Close())
	require.NoError(t, ch.Close()) // idempotent
	assert.False(t, ch.IsConnected())
}

func TestConnectCancelledContext(t *testing.T) {
	mustPolicy(t, t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := New().Connect(ctx)
	require.Error(t, err)
}

// --- registry integration --------------------------------------------------------

func TestRegisteredInDefaultRegistry(t *testing.T) {
	f, ok := channel.DefaultRegistry().Factory("local")
	require.True(t, ok, "local factory must be registered at init")
	ch, err := f.Create(Target{Hostname: "localhost"})
	require.NoError(t, err)
	require.NotNil(t, ch)
}

func TestRegistryCreateLocalTarget(t *testing.T) {
	ch, err := channel.DefaultRegistry().Create(Target{Hostname: "sandbox-1"})
	require.NoError(t, err)
	require.NotNil(t, ch)
}

// --- concurrency -------------------------------------------------------------------

func TestConcurrentExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("platform gate covered by TestExecRunsAllowedCommand")
	}
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ch.Exec(context.Background(), "echo concurrent")
			if err != nil || res.ExitCode != 0 {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent exec failed: %v", err)
	}
}

// --- platform gate -----------------------------------------------------------

// TestExecPlatformGate pins that Exec on windows is refused at the Exec
// entry (not via convention), while CheckPolicy stays pure and tests the
// allow-list on every platform.
func TestExecPlatformGate(t *testing.T) {
	mustPolicy(t, t.TempDir(), map[string]ArgPolicy{"echo": ArgShell})
	ctx := context.Background()

	if runtime.GOOS == "windows" {
		_, err := New().Exec(ctx, "echo hi")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not supported on windows")
	} else {
		require.NoError(t, CheckPolicy(ctx, "echo", []string{"hi"}))
	}
}

// --- buffer sanity (guards the io plumbing) ---------------------------------------

func TestUploadLargeContent(t *testing.T) {
	dir := t.TempDir()
	mustPolicy(t, dir, nil)

	ch := New()
	require.NoError(t, ch.Connect(context.Background()))
	defer func() { _ = ch.Close() }()

	big := bytes.Repeat([]byte("0123456789"), 100_000) // 1MB
	require.NoError(t, ch.Upload(context.Background(), "big.bin", bytes.NewReader(big)))

	r, err := ch.Download(context.Background(), "big.bin")
	require.NoError(t, err)
	defer func() { _ = r.(io.Closer).Close() }()
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Len(t, got, len(big))
}
