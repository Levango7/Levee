package ssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/nexus/levee/internal/channel"
)

// --- mock SSH server -------------------------------------------------------

// mockServer is a minimal in-process SSH server used by the tests. It speaks
// just enough of the protocol to exercise Connect / Exec / Upload / Download:
//   - password auth with a single fixed credential;
//   - exec of arbitrary commands via the default session handler, with the
//     command run through the local shell so that `cat`, `echo`, etc. work;
//   - stdin piping so `cat > path` uploads files to the local temp dir.
type mockServer struct {
	t        testing.TB
	listener net.Listener
	addr     string
	password string
	username string

	mu      sync.Mutex
	execLog []string
	closed  bool
}

// newMockServer starts a mock SSH server on a random localhost port. The
// caller must defer srv.Close().
func newMockServer(t testing.TB, username, password string) *mockServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	require.NoError(t, err)

	srv := &mockServer{
		t:        t,
		listener: ln,
		addr:     ln.Addr().String(),
		username: username,
		password: password,
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
			if c.User() == username && string(pass) == password {
				return nil, nil
			}
			return nil, fmt.Errorf("mock: bad credentials")
		},
	}
	config.AddHostKey(hostSigner)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.handleConn(conn, config)
		}
	}()

	return srv
}

// handleConn serves a single SSH connection.
func (s *mockServer) handleConn(nconn net.Conn, config *ssh.ServerConfig) {
	conn, chans, reqs, err := ssh.NewServerConn(nconn, config)
	if err != nil {
		return
	}
	defer conn.Close()

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.handleSession(channel, requests)
	}
}

// handleSession serves a single "session" channel. It supports exec and
// stdin piping for `cat > path`. Channel data (sent via session.StdinPipe
// on the client side) is collected as the command's stdin.
func (s *mockServer) handleSession(ch ssh.Channel, requests <-chan *ssh.Request) {
	defer ch.Close()

	for req := range requests {
		switch req.Type {
		case "exec":
			// Parse the command. Wire format: 4-byte length + string.
			if len(req.Payload) < 4 {
				_ = req.Reply(false, nil)
				continue
			}
			cmdLen := int(req.Payload[0])<<24 | int(req.Payload[1])<<16 | int(req.Payload[2])<<8 | int(req.Payload[3])
			if len(req.Payload) < 4+cmdLen {
				_ = req.Reply(false, nil)
				continue
			}
			execCmd := string(req.Payload[4 : 4+cmdLen])
			s.mu.Lock()
			s.execLog = append(s.execLog, execCmd)
			s.mu.Unlock()
			_ = req.Reply(true, nil)

			// Read all stdin data from the channel. Channel.Read returns
			// io.EOF when the client closes stdin (sends SSH EOF). We use
			// a short read loop with a timeout so commands that never
			// receive stdin don't block forever.
			var stdinBuf bytes.Buffer
			readDone := make(chan struct{})
			go func() {
				_, _ = io.Copy(&stdinBuf, ch)
				close(readDone)
			}()
			select {
			case <-readDone:
			case <-time.After(2 * time.Second):
			}

			exitCode, stdout, stderr := s.runCommand(execCmd, &stdinBuf)
			_, _ = ch.Write(stdout)
			if len(stderr) > 0 {
				_, _ = ch.SendRequest("stderr", false, stderr)
			}
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ C uint32 }{uint32(exitCode)}))
			// Close the channel to signal command completion; this lets
			// the client's session.Wait() return promptly.
			_ = ch.Close()
			return

		case "shell":
			_ = req.Reply(false, nil)

		case "pty-req":
			_ = req.Reply(false, nil)

		case "eof":
			_ = req.Reply(true, nil)

		default:
			_ = req.Reply(false, nil)
		}
	}
}

// runCommand executes cmd against the local OS and returns exit code, stdout,
// stderr. It uses a built-in interpreter for the subset of commands the tests
// rely on (cat, echo) so it works on Windows without a POSIX shell.
func (s *mockServer) runCommand(cmd string, stdin *bytes.Buffer) (int, []byte, []byte) {
	cmd = s.translatePaths(cmd)
	if out, ok := s.builtinCommand(cmd, stdin); ok {
		return 0, out, nil
	}
	// Fall back to the system shell for anything we don't recognise.
	var stdout, stderr bytes.Buffer
	c := execCommand(cmd)
	if c == nil {
		return 127, nil, []byte("mock: command not understood: " + cmd)
	}
	c.Stdin = bytes.NewReader(stdin.Bytes())
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return 1, stdout.Bytes(), stderr.Bytes()
	}
	return 0, stdout.Bytes(), stderr.Bytes()
}

// translatePaths rewrites /tmp/... paths in cmd to use the local temp dir.
// This lets the tests use POSIX-style paths even on Windows.
func (s *mockServer) translatePaths(cmd string) string {
	tmp := os.TempDir()
	// Replace /tmp with the local temp dir.
	cmd = strings.ReplaceAll(cmd, "/tmp", strings.ReplaceAll(tmp, "\\", "/"))
	return cmd
}

// builtinCommand handles the small subset of commands the tests use without
// invoking a real shell. Returns (output, true) when the command was handled.
func (s *mockServer) builtinCommand(cmd string, stdin *bytes.Buffer) ([]byte, bool) {
	cmd = strings.TrimSpace(cmd)
	// echo <text>
	if strings.HasPrefix(cmd, "echo ") {
		text := strings.TrimPrefix(cmd, "echo ")
		// Strip surrounding quotes if present.
		text = strings.Trim(text, "\"")
		return []byte(text + "\n"), true
	}
	if cmd == "echo" {
		return []byte("\n"), true
	}
	// cat <path>
	if strings.HasPrefix(cmd, "cat ") && !strings.HasPrefix(cmd, "cat >") {
		path := strings.TrimSpace(strings.TrimPrefix(cmd, "cat "))
		path = strings.Trim(path, "'\"")
		data, err := os.ReadFile(path)
		if err != nil {

			return nil, false
		}
		return data, true
	}
	// cat > <path>  -- write stdin to path
	if strings.HasPrefix(cmd, "cat > ") {
		path := strings.TrimSpace(strings.TrimPrefix(cmd, "cat > "))
		path = strings.Trim(path, "'\"")
		if err := os.WriteFile(path, stdin.Bytes(), 0o644); err != nil {
			return nil, false
		}
		return nil, true
	}
	// test/true
	if cmd == "true" || strings.HasPrefix(cmd, "test ") {
		return nil, true
	}
	// exit <code>
	if strings.HasPrefix(cmd, "exit ") {
		// We can't return a non-zero exit code from here; the caller
		// (execLocal) will treat any handled command as success. The
		// tests that need a non-zero exit code use a different path.
		return nil, true
	}
	return nil, false
}

// Close stops the mock server.
func (s *mockServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.listener.Close()
}

// execCommand returns an *exec.Cmd for running cmd through the system shell.
// On POSIX systems it uses /bin/sh -c; on Windows it uses cmd.exe /c.
func execCommand(cmd string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd.exe", "/c", cmd)
	}
	return exec.Command("/bin/sh", "-c", cmd)
}

// Addr returns the host:port the server is listening on.
func (s *mockServer) Addr() string { return s.addr }

// ExecLog returns a snapshot of the commands the server has executed.
func (s *mockServer) ExecLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.execLog))
	copy(out, s.execLog)
	return out
}

// --- test helpers ----------------------------------------------------------

// staticTarget is a minimal channel.Target implementation for tests.
type staticTarget struct {
	host string
	port int
	typ  string
	cred channel.CredentialRef
}

func (t staticTarget) Host() string                       { return t.host }
func (t staticTarget) Port() int                          { return t.port }
func (t staticTarget) Type() string                       { return t.typ }
func (t staticTarget) Credentials() channel.CredentialRef { return t.cred }

// splitHostPort splits s.addr into host and port for the test target.
func splitHostPort(t testing.TB, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port := 0
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)
	return host, port
}

// --- SSHChannel tests ------------------------------------------------------

func TestSSHFactoryCreate(t *testing.T) {
	f := &SSHFactory{}
	tgt := staticTarget{host: "127.0.0.1", port: 22, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	ch, err := f.Create(tgt)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.False(t, ch.IsConnected())
}

func TestSSHFactoryCreateWrongType(t *testing.T) {
	f := &SSHFactory{}
	tgt := staticTarget{host: "127.0.0.1", port: 22, typ: "winrm"}
	_, err := f.Create(tgt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "winrm")
}

func TestSSHFactoryCreateNilTarget(t *testing.T) {
	f := &SSHFactory{}
	_, err := f.Create(nil)
	require.Error(t, err)
}

func TestSSHChannelConnectPassword(t *testing.T) {
	srv := newMockServer(t, "testuser", "testpass")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{
		host: host,
		port: port,
		typ:  "ssh",
		cred: channel.CredentialRef{Username: "testuser", Password: "testpass"},
	}
	cfg := NewConfig()
	cfg.StrictHostCheck = false // lab mode

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))
	assert.True(t, ch.IsConnected())
}

func TestSSHChannelConnectBadPassword(t *testing.T) {
	srv := newMockServer(t, "testuser", "testpass")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{
		host: host,
		port: port,
		typ:  "ssh",
		cred: channel.CredentialRef{Username: "testuser", Password: "wrongpass"},
	}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = ch.Connect(ctx)
	require.Error(t, err)
	assert.False(t, ch.IsConnected())
}

func TestSSHChannelConnectIdempotent(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx := context.Background()
	require.NoError(t, ch.Connect(ctx))
	require.NoError(t, ch.Connect(ctx)) // second call is a no-op
	assert.True(t, ch.IsConnected())
}

func TestSSHChannelExecSuccess(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))

	res, err := ch.Exec(ctx, "echo hello")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello\n", res.Stdout)
	assert.True(t, res.Duration >= 0)
}

func TestSSHChannelExecNotConnected(t *testing.T) {
	tgt := staticTarget{host: "127.0.0.1", port: 22, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	ch, err := NewChannel(tgt, NewConfig())
	require.NoError(t, err)
	defer ch.Close()

	_, err = ch.Exec(context.Background(), "echo hi")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestSSHChannelExecCancelled(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))

	// Cancel immediately; the exec should return ctx.Err().
	cancelCtx, cancelExec := context.WithCancel(context.Background())
	cancelExec()
	_, err = ch.Exec(cancelCtx, "echo hi")
	// Either an error or a fast success is acceptable here since the
	// command may complete before the cancellation is observed.
	if err != nil {
		assert.Contains(t, err.Error(), "context canceled")
	}
}

func TestSSHChannelUploadDownload(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))

	// Use a temp file path that the mock server can write to.
	tmpDir := t.TempDir()
	remotePath := filepath.ToSlash(filepath.Join(tmpDir, "uploaded.txt"))
	content := []byte("hello ssh world\n")

	// Upload
	err = ch.Upload(ctx, remotePath, bytes.NewReader(content))
	require.NoError(t, err)

	// Verify the file was written locally (the mock server writes to the
	// local filesystem).
	got, err := os.ReadFile(filepath.FromSlash(remotePath))
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// Download
	r, err := ch.Download(ctx, remotePath)
	require.NoError(t, err)
	// The concrete type implements io.Closer; close it to release the session.
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	downloaded, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)
}

func TestSSHChannelClose(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))
	assert.True(t, ch.IsConnected())

	require.NoError(t, ch.Close())
	assert.False(t, ch.IsConnected())

	// Close is idempotent.
	require.NoError(t, ch.Close())
}

func TestSSHChannelConnectCancelledContext(t *testing.T) {
	// Use a non-routable address so the dial blocks, then cancel.
	tgt := staticTarget{host: "10.255.255.1", port: 22, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	cfg := NewConfig()
	cfg.StrictHostCheck = false
	cfg.ConnectTimeout = 30 * time.Second

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err = ch.Connect(ctx)
	require.Error(t, err)
}

// --- key auth tests --------------------------------------------------------

func TestSSHChannelConnectKeyAuth(t *testing.T) {
	// Generate an ed25519 key for the test.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	// Start a mock server that accepts this key.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	hostSigner, err := ssh.NewSignerFromKey(hostKey)
	require.NoError(t, err)

	pubKey, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), pubKey.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("mock: unknown key")
		},
	}
	config.AddHostKey(hostSigner)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(nconn net.Conn) {
				conn, chans, reqs, err := ssh.NewServerConn(nconn, config)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(reqs)
				for newChannel := range chans {
					if newChannel.ChannelType() != "session" {
						_ = newChannel.Reject(ssh.UnknownChannelType, "")
						continue
					}
					channel, requests, err := newChannel.Accept()
					if err != nil {
						return
					}
					go func() {
						defer channel.Close()
						for req := range requests {
							_ = req.Reply(false, nil)
						}
					}()
				}
			}(conn)
		}
	}()

	// Write the private key to a temp file in PEM format.
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pemBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600))

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	tgt := staticTarget{
		host: host,
		port: port,
		typ:  "ssh",
		cred: channel.CredentialRef{Username: "keyuser", KeyPath: keyPath},
	}
	cfg := NewConfig()
	cfg.StrictHostCheck = false
	cfg.AuthMethod = "key"

	ch, err := NewChannel(tgt, cfg)
	require.NoError(t, err)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))
	assert.True(t, ch.IsConnected())
}

// --- SSHPool tests ---------------------------------------------------------

func TestSSHPoolGetAndPut(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        3,
		IdleTimeout:         1 * time.Second,
		HealthCheckInterval: 100 * time.Millisecond,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.True(t, ch.IsConnected())

	// Return the connection; it should now be idle in the pool.
	pool.Put(ch)

	stats := pool.Stats()
	assert.Equal(t, 1, stats.Targets)
	assert.Equal(t, 1, stats.TotalConns)
	assert.Equal(t, 1, stats.IdleConns)
}

func TestSSHPoolReuse(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        2,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch1, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	pool.Put(ch1)

	// Second Get should reuse the idle connection rather than opening a new one.
	ch2, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	assert.Same(t, ch1, ch2, "pool should reuse the idle connection")

	stats := pool.Stats()
	assert.Equal(t, 1, stats.TotalConns, "no new connection should be opened")
}

func TestSSHPoolMultipleConcurrent(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        3,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Acquire 3 connections concurrently; all should succeed.
	var conns [3]*SSHChannel
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ch, err := pool.Get(ctx, tgt)
			require.NoError(t, err)
			conns[i] = ch
		}(i)
	}
	wg.Wait()

	stats := pool.Stats()
	assert.Equal(t, 3, stats.TotalConns)
	assert.Equal(t, 3, stats.InUseConns)

	for _, ch := range conns {
		pool.Put(ch)
	}
	stats = pool.Stats()
	assert.Equal(t, 3, stats.IdleConns)
}

func TestSSHPoolMaxPerTargetBlocks(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        1,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Acquire the only allowed connection.
	ch1, err := pool.Get(ctx, tgt)
	require.NoError(t, err)

	// A second Get should block; use a short timeout so the test does not hang.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer shortCancel()
	_, err = pool.Get(shortCtx, tgt)
	require.Error(t, err) // timed out waiting for a slot

	// Return the first connection; now Get should succeed.
	pool.Put(ch1)
	_, err = pool.Get(ctx, tgt)
	require.NoError(t, err)
}

func TestSSHPoolIdleReaping(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        2,
		IdleTimeout:         100 * time.Millisecond,
		HealthCheckInterval: 50 * time.Millisecond,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	pool.Put(ch)

	// Wait for the reaper to close the idle connection.
	require.Eventually(t, func() bool {
		return pool.Stats().TotalConns == 0
	}, 2*time.Second, 50*time.Millisecond, "idle connection should be reaped")
}

func TestSSHPoolClose(t *testing.T) {
	srv := newMockServer(t, "u", "p")
	defer srv.Close()

	host, port := splitHostPort(t, srv.Addr())
	tgt := staticTarget{host: host, port: port, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        2,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	pool.Put(ch)

	require.NoError(t, pool.Close())
	// Close is idempotent.
	require.NoError(t, pool.Close())
	assert.False(t, ch.IsConnected())
}

func TestSSHPoolGetAfterClose(t *testing.T) {
	pool := NewPool(PoolConfig{})
	require.NoError(t, pool.Close())

	tgt := staticTarget{host: "127.0.0.1", port: 22, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	_, err := pool.Get(context.Background(), tgt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "closed")
}

func TestSSHPoolMultipleTargets(t *testing.T) {
	srv1 := newMockServer(t, "u", "p")
	defer srv1.Close()
	srv2 := newMockServer(t, "u", "p")
	defer srv2.Close()

	host1, port1 := splitHostPort(t, srv1.Addr())
	host2, port2 := splitHostPort(t, srv2.Addr())

	tgt1 := staticTarget{host: host1, port: port1, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}
	tgt2 := staticTarget{host: host2, port: port2, typ: "ssh", cred: channel.CredentialRef{Username: "u", Password: "p"}}

	relaxHostChecking(t)
	pool := NewPool(PoolConfig{
		MaxPerTarget:        2,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch1, err := pool.Get(ctx, tgt1)
	require.NoError(t, err)
	ch2, err := pool.Get(ctx, tgt2)
	require.NoError(t, err)
	assert.NotSame(t, ch1, ch2)

	stats := pool.Stats()
	assert.Equal(t, 2, stats.Targets)
	assert.Equal(t, 2, stats.TotalConns)

	pool.Put(ch1)
	pool.Put(ch2)
}

// --- config / defaults tests -----------------------------------------------

// relaxHostChecking switches the process-wide SSH defaults to insecure mode
// for the duration of the test, restoring the previous defaults afterwards.
// Pool tests need this because they dial the in-process mock server whose
// host key is not in any known_hosts file.
func relaxHostChecking(tb testing.TB) {
	tb.Helper()
	prevStrict, prevKnownHosts := currentDefaults()
	SetDefaultConfig(false, "")
	tb.Cleanup(func() { SetDefaultConfig(prevStrict, prevKnownHosts) })
}

func TestSetDefaultConfigRoundTrip(t *testing.T) {
	prevStrict, prevKnownHosts := currentDefaults()
	t.Cleanup(func() { SetDefaultConfig(prevStrict, prevKnownHosts) })

	// Explicit opt-out propagates into NewConfig.
	SetDefaultConfig(false, "/tmp/kh_known")
	cfg := NewConfig()
	assert.False(t, cfg.StrictHostCheck)
	assert.Equal(t, "/tmp/kh_known", cfg.KnownHostsPath)

	// Strict with an explicit path propagates verbatim.
	SetDefaultConfig(true, "/tmp/kh_strict")
	cfg = NewConfig()
	assert.True(t, cfg.StrictHostCheck)
	assert.Equal(t, "/tmp/kh_strict", cfg.KnownHostsPath)

	// Strict with an empty path auto-detects ~/.ssh/known_hosts.
	SetDefaultConfig(true, "")
	cfg = NewConfig()
	assert.True(t, cfg.StrictHostCheck)
	assert.Equal(t, DefaultKnownHostsPath(), cfg.KnownHostsPath)
}

func TestNewConfig(t *testing.T) {
	prevStrict, prevKnownHosts := currentDefaults()
	t.Cleanup(func() { SetDefaultConfig(prevStrict, prevKnownHosts) })
	SetDefaultConfig(true, "")

	cfg := NewConfig()
	assert.Equal(t, 0, cfg.Port, "NewConfig should leave Port zero so Target.Port() is honoured")
	assert.Equal(t, DefaultConnectTimeout, cfg.ConnectTimeout)
	assert.True(t, cfg.StrictHostCheck, "NewConfig must default to strict host checking (secure-by-default)")
	assert.NotEmpty(t, cfg.KnownHostsPath, "known_hosts path must be auto-detected when unset")
	assert.Contains(t, cfg.KnownHostsPath, ".ssh")
}

func TestPoolConfigWithDefaults(t *testing.T) {
	cfg := PoolConfig{}.withDefaults()
	assert.Equal(t, DefaultMaxPerTarget, cfg.MaxPerTarget)
	assert.Equal(t, DefaultIdleTimeout, cfg.IdleTimeout)
	assert.Equal(t, DefaultHealthCheckInterval, cfg.HealthCheckInterval)
}

func TestShellQuote(t *testing.T) {
	assert.Equal(t, "'hello'", shellQuote("hello"))
	assert.Equal(t, `'it'\''s'`, shellQuote("it's"))
	assert.Equal(t, `'/tmp/a b.txt'`, shellQuote("/tmp/a b.txt"))
}

func TestEffectivePort(t *testing.T) {
	// Config override takes precedence.
	ch := &SSHChannel{
		target: staticTarget{host: "h", port: 2222, typ: "ssh"},
		cfg:    &Config{Port: 2222},
	}
	assert.Equal(t, 2222, ch.effectivePort())

	// Target port used when config port is zero.
	ch = &SSHChannel{
		target: staticTarget{host: "h", port: 2222, typ: "ssh"},
		cfg:    &Config{},
	}
	assert.Equal(t, 2222, ch.effectivePort())

	// Default port when both are zero.
	ch = &SSHChannel{
		target: staticTarget{host: "h", port: 0, typ: "ssh"},
		cfg:    &Config{},
	}
	assert.Equal(t, DefaultPort, ch.effectivePort())
}

func TestPoolKey(t *testing.T) {
	tgt := staticTarget{host: "10.0.0.1", port: 22, typ: "ssh"}
	assert.Equal(t, "10.0.0.1:22", poolKey(tgt))

	tgt = staticTarget{host: "10.0.0.1", port: 0, typ: "ssh"}
	assert.Equal(t, "10.0.0.1:22", poolKey(tgt))
}

// --- host-key callback selection tests --------------------------------------

// callbackFor builds a channel for cfg and returns its resolved host-key
// callback via buildHostKeyCallback.
func callbackFor(t *testing.T, cfg *Config) (ssh.HostKeyCallback, error) {
	t.Helper()
	ch := &SSHChannel{
		target: staticTarget{host: "127.0.0.1", port: 22, typ: "ssh"},
		cfg:    cfg,
	}
	return ch.buildHostKeyCallback()
}

func TestBuildHostKeyCallbackSelectionOverrideWins(t *testing.T) {
	prevStrict, prevKnownHosts := currentDefaults()
	t.Cleanup(func() { SetDefaultConfig(prevStrict, prevKnownHosts) })
	SetDefaultConfig(true, "")

	var calls int
	override := func(string, net.Addr, ssh.PublicKey) error { calls++; return nil }

	cb, err := callbackFor(t, &Config{StrictHostCheck: true, KnownHostsPath: "/nonexistent", HostKeyCallback: override})
	require.NoError(t, err)
	key := mockPublicKey(t)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
	require.NoError(t, cb("127.0.0.1", addr, key))
	assert.Equal(t, 1, calls, "explicit HostKeyCallback must take precedence over known_hosts")
}

func TestBuildHostKeyCallbackInsecureAcceptsAnyKey(t *testing.T) {
	cb, err := callbackFor(t, &Config{StrictHostCheck: false})
	require.NoError(t, err)
	require.NotNil(t, cb)
	// Insecure mode must accept a key that appears in no known_hosts file.
	key := mockPublicKey(t)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
	assert.NoError(t, cb("some-unknown-host.example.com", addr, key))
}

func TestBuildHostKeyCallbackStrictNoPathFails(t *testing.T) {
	cb, err := callbackFor(t, &Config{StrictHostCheck: true, KnownHostsPath: ""})
	require.Error(t, err)
	assert.Nil(t, cb)
	assert.Contains(t, err.Error(), "known_hosts")
}

func TestBuildHostKeyCallbackStrictMissingFileMentionsKeyscan(t *testing.T) {
	cb, err := callbackFor(t, &Config{StrictHostCheck: true, KnownHostsPath: filepath.Join(t.TempDir(), "does_not_exist")})
	require.Error(t, err)
	assert.Nil(t, cb)
	assert.Contains(t, err.Error(), "ssh-keyscan")
}

// writeKnownHosts creates a temporary known_hosts file trusting pubKey for
// addr, returning the file path.
func writeKnownHosts(t *testing.T, addrs []net.Addr, key ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	hostPorts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		hostPorts = append(hostPorts, a.String())
	}
	line := knownhosts.Line(hostPorts, key)
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o600))
	return path
}

func mockPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pk, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return pk
}

func TestStrictHostCheckAgainstKnownHostsFile(t *testing.T) {
	trustedKey := mockPublicKey(t)
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}
	path := writeKnownHosts(t, []net.Addr{addr}, trustedKey)

	cb, err := callbackFor(t, &Config{StrictHostCheck: true, KnownHostsPath: path})
	require.NoError(t, err)

	// The trusted host+key combination verifies. Note that the SSH handshake
	// delivers the hostname in host:port form.
	assert.NoError(t, cb("127.0.0.1:22", addr, trustedKey))

	// A different key for a known host (possible MITM / key rotation) fails
	// and the error explains how to provision the new key.
	otherKey := mockPublicKey(t)
	err = cb("127.0.0.1:22", addr, otherKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ssh-keyscan")

	// An unlisted host fails too.
	err = cb("unlisted-host.example.com:22", addr, trustedKey)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ssh-keyscan")
}
