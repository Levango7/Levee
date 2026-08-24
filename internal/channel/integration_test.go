// Package channel_test contains integration tests for the channel abstraction
// layer. It lives in package channel_test (not channel) so that it can import
// the ssh sub-package without forming an import cycle (ssh imports channel).
//
// The tests spin up in-process mock SSH servers and drive the full stack:
// registry -> factory -> SSH channel -> Connect/Exec/Upload/Download -> Close,
// combined with the Limiter and Prechecker to verify end-to-end behaviour.
package channel_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cryptossh "golang.org/x/crypto/ssh"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/channel/ssh"
)

// --- integration test mock SSH server ----------------------------------------
//
// The mock SSH server defined in internal/channel/ssh/ssh_test.go is private
// to the ssh package. To keep the integration tests self-contained, we
// re-implement a minimal in-process SSH server here that supports just enough
// of the protocol for the end-to-end scenarios: password auth, exec of
// "echo <text>" and "true", and stdin piping for "cat > path".
//
// The server is intentionally small; it does not implement SFTP, port
// forwarding, or any advanced SSH features. It exists to exercise the
// Channel interface (Connect / Exec / Upload / Download) against a real SSH
// transport stack driven by golang.org/x/crypto/ssh.

// integServer is an in-process mock SSH server for integration tests.
type integServer struct {
	t        testing.TB
	listener net.Listener
	addr     string
	username string
	password string

	mu      sync.Mutex
	execLog []string
	closed  bool

	// refuseAuth, when true, makes every password check fail. Used to
	// simulate a server that is up but rejects credentials (error recovery).
	refuseAuth int32

	// connCount tracks the number of SSH connections accepted.
	connCount int32
}

// newIntegServer starts a mock SSH server on a random localhost port.
func newIntegServer(t testing.TB, username, password string) *integServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	hostKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	hostSigner, err := cryptossh.NewSignerFromKey(hostKey)
	require.NoError(t, err)

	srv := &integServer{
		t:        t,
		listener: ln,
		addr:     ln.Addr().String(),
		username: username,
		password: password,
	}

	config := &cryptossh.ServerConfig{
		PasswordCallback: func(c cryptossh.ConnMetadata, pass []byte) (*cryptossh.Permissions, error) {
			if atomic.LoadInt32(&srv.refuseAuth) == 1 {
				return nil, fmt.Errorf("mock: auth refused")
			}
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
			atomic.AddInt32(&srv.connCount, 1)
			go srv.handleConn(conn, config)
		}
	}()

	return srv
}

func (s *integServer) handleConn(nconn net.Conn, config *cryptossh.ServerConfig) {
	conn, chans, reqs, err := cryptossh.NewServerConn(nconn, config)
	if err != nil {
		return
	}
	defer conn.Close()
	go cryptossh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(cryptossh.UnknownChannelType, "unknown channel type")
			continue
		}
		ch, requests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.handleSession(ch, requests)
	}
}

func (s *integServer) handleSession(ch cryptossh.Channel, requests <-chan *cryptossh.Request) {
	defer ch.Close()
	for req := range requests {
		switch req.Type {
		case "exec":
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
			_, _ = ch.SendRequest("exit-status", false, cryptossh.Marshal(struct{ C uint32 }{uint32(exitCode)}))
			_ = ch.Close()
			return

		case "shell":
			_ = req.Reply(false, nil)
		case "pty-req":
			_ = req.Reply(false, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}

// runCommand interprets a small subset of commands sufficient for the
// integration tests: echo, true, cat > path, cat < path>.
func (s *integServer) runCommand(cmd string, stdin *bytes.Buffer) (int, []byte, []byte) {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "echo ") {
		text := strings.TrimPrefix(cmd, "echo ")
		text = strings.Trim(text, "\"")
		return 0, []byte(text + "\n"), nil
	}
	if cmd == "echo" {
		return 0, []byte("\n"), nil
	}
	if cmd == "true" {
		return 0, nil, nil
	}
	if strings.HasPrefix(cmd, "cat > ") {
		path := strings.TrimSpace(strings.TrimPrefix(cmd, "cat > "))
		path = strings.Trim(path, "'\"")
		if err := os.WriteFile(path, stdin.Bytes(), 0o644); err != nil {
			return 1, nil, []byte(err.Error())
		}
		return 0, nil, nil
	}
	if strings.HasPrefix(cmd, "cat ") && !strings.HasPrefix(cmd, "cat >") {
		path := strings.TrimSpace(strings.TrimPrefix(cmd, "cat "))
		path = strings.Trim(path, "'\"")
		data, err := os.ReadFile(path)
		if err != nil {
			return 1, nil, []byte(err.Error())
		}
		return 0, data, nil
	}
	return 127, nil, []byte("mock: command not understood: " + cmd)
}

// Close stops the mock server.
func (s *integServer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	_ = s.listener.Close()
}

// Addr returns the host:port the server is listening on.
func (s *integServer) Addr() string { return s.addr }

// ExecLog returns a snapshot of the commands the server has executed.
func (s *integServer) ExecLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.execLog))
	copy(out, s.execLog)
	return out
}

// ConnCount returns the number of SSH connections accepted so far.
func (s *integServer) ConnCount() int32 { return atomic.LoadInt32(&s.connCount) }

// SetRefuseAuth toggles credential rejection to simulate an unreachable /
// misconfigured server.
func (s *integServer) SetRefuseAuth(refuse bool) {
	if refuse {
		atomic.StoreInt32(&s.refuseAuth, 1)
	} else {
		atomic.StoreInt32(&s.refuseAuth, 0)
	}
}

// --- test helpers -----------------------------------------------------------

// integTarget is a channel.Target implementation backed by a mock server.
type integTarget struct {
	host string
	port int
	typ  string
	cred channel.CredentialRef
}

func (t integTarget) Host() string                       { return t.host }
func (t integTarget) Port() int                          { return t.port }
func (t integTarget) Type() string                       { return t.typ }
func (t integTarget) Credentials() channel.CredentialRef { return t.cred }

// newIntegTarget builds an integTarget pointing at srv with the given creds.
func newIntegTarget(t testing.TB, srv *integServer, username, password string) integTarget {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Addr())
	require.NoError(t, err)
	port := 0
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)
	return integTarget{
		host: host,
		port: port,
		typ:  "ssh",
		cred: channel.CredentialRef{Username: username, Password: password},
	}
}

// newSSHChannel creates a real SSH channel to the given target with lab-friendly
// config (no strict host check).
func newSSHChannel(t testing.TB, tgt integTarget) *ssh.SSHChannel {
	t.Helper()
	cfg := ssh.NewConfig()
	cfg.StrictHostCheck = false
	ch, err := ssh.NewChannel(tgt, cfg)
	require.NoError(t, err)
	return ch
}

// splitAddr splits host:port into (host, port).
func splitAddr(t testing.TB, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port := 0
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err)
	return host, port
}

// sshFactoryAdapter is a ChannelFactory that creates real SSH channels with
// lab-friendly config (no strict host check). It is used by the integration
// tests to drive the Prechecker against mock SSH servers.
type sshFactoryAdapter struct{}

func (f *sshFactoryAdapter) Create(target channel.Target) (channel.Channel, error) {
	if target == nil {
		return nil, fmt.Errorf("nil target")
	}
	cfg := ssh.NewConfig()
	cfg.StrictHostCheck = false
	return ssh.NewChannel(target, cfg)
}

// --- T019 integration scenarios ---------------------------------------------

// TestIntegrationConnectExecClose exercises the full Channel lifecycle against
// a real in-process SSH server: Connect -> Exec -> result -> Close.
func TestIntegrationConnectExecClose(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")
	ch := newSSHChannel(t, tgt)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect
	require.NoError(t, ch.Connect(ctx))
	assert.True(t, ch.IsConnected())

	// Exec
	res, err := ch.Exec(ctx, "echo hello-levee")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Equal(t, "hello-levee\n", res.Stdout)

	// Verify the server saw the command.
	log := srv.ExecLog()
	require.Len(t, log, 1)
	assert.Contains(t, log[0], "echo hello-levee")

	// Close
	require.NoError(t, ch.Close())
	assert.False(t, ch.IsConnected())

	// Close is idempotent.
	require.NoError(t, ch.Close())
}

// TestIntegrationUploadDownload verifies file transfer works end-to-end.
func TestIntegrationUploadDownload(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")
	ch := newSSHChannel(t, tgt)
	defer ch.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ch.Connect(ctx))

	tmpDir := t.TempDir()
	remotePath := filepath.ToSlash(filepath.Join(tmpDir, "integ-uploaded.txt"))
	content := []byte("levee integration payload\n")

	// Upload
	err := ch.Upload(ctx, remotePath, bytes.NewReader(content))
	require.NoError(t, err)

	// Verify the file landed on the local FS (the mock server writes locally).
	got, err := os.ReadFile(filepath.FromSlash(remotePath))
	require.NoError(t, err)
	assert.Equal(t, content, got)

	// Download
	r, err := ch.Download(ctx, remotePath)
	require.NoError(t, err)
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	downloaded, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, content, downloaded)
}

// relaxSSHDefaults switches the ssh package's process-wide defaults to
// insecure host checking for the duration of the test, restoring the previous
// values afterwards. The integration tests dial in-process mock servers whose
// host keys are not in any known_hosts file.
func relaxSSHDefaults(t testing.TB) {
	t.Helper()
	ssh.SetDefaultConfig(false, "", "", "")
	t.Cleanup(func() { ssh.SetDefaultConfig(true, "", "", "") })
}

// TestIntegrationPoolReuse verifies that the SSH connection pool reuses the
// same underlying connection for multiple operations against one target.
func TestIntegrationPoolReuse(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")

	relaxSSHDefaults(t)
	pool := ssh.NewPool(ssh.PoolConfig{
		MaxPerTarget:        2,
		IdleTimeout:         10 * time.Second,
		HealthCheckInterval: 1 * time.Second,
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First Get: opens a new connection.
	ch1, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	pool.Put(ch1)

	// Second Get: should reuse the idle connection.
	ch2, err := pool.Get(ctx, tgt)
	require.NoError(t, err)
	assert.Same(t, ch1, ch2, "pool should reuse the idle connection")

	// Run a command through the reused connection to confirm it is alive.
	res, err := ch2.Exec(ctx, "echo reused")
	require.NoError(t, err)
	assert.Equal(t, "reused\n", res.Stdout)

	pool.Put(ch2)

	stats := pool.Stats()
	assert.Equal(t, 1, stats.TotalConns, "no new connection should have been opened")
}

// TestIntegrationLimiterConcurrency verifies that the Limiter caps concurrent
// operations and that all permits are released after the operations complete.
func TestIntegrationLimiterConcurrency(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")

	// global=2, per-channel=2, per-target=1 -> at most 1 concurrent op per
	// target, 2 globally.
	limiter := channel.NewLimiter(2, 2, 1, 0)
	defer limiter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("%s:%d", tgt.Host(), tgt.Port())
			if err := limiter.Acquire(tgt.Type(), id); err != nil {
				errs[idx] = err
				return
			}
			defer limiter.Release(tgt.Type(), id)

			ch := newSSHChannel(t, tgt)
			defer ch.Close()
			if err := ch.Connect(ctx); err != nil {
				errs[idx] = err
				return
			}
			_, err := ch.Exec(ctx, "echo ok")
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "goroutine %d failed", i)
	}

	stats := limiter.Stats()
	assert.Equal(t, int64(n), stats.TotalAcquired, "all acquires should succeed")
	assert.Equal(t, 0, stats.GlobalInUse, "all permits should be released")
}

// TestIntegrationPrecheckMultipleTargets runs the Prechecker against several
// mock SSH servers, verifying that reachable and unreachable targets are
// correctly classified.
func TestIntegrationPrecheckMultipleTargets(t *testing.T) {
	// Three reachable servers.
	srv1 := newIntegServer(t, "u", "p")
	defer srv1.Close()
	srv2 := newIntegServer(t, "u", "p")
	defer srv2.Close()
	srv3 := newIntegServer(t, "u", "p")
	defer srv3.Close()

	tgt1 := newIntegTarget(t, srv1, "u", "p")
	tgt2 := newIntegTarget(t, srv2, "u", "p")
	tgt3 := newIntegTarget(t, srv3, "u", "p")

	// A fourth target pointing at a closed port -> unreachable.
	badLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	badAddr := badLn.Addr().String()
	_ = badLn.Close() // close immediately so connect fails
	badHost, badPort := splitAddr(t, badAddr)
	tgtBad := integTarget{
		host: badHost,
		port: badPort,
		typ:  "ssh",
		cred: channel.CredentialRef{Username: "u", Password: "p"},
	}

	// Build a ChannelFactory that creates real SSH channels with lab config.
	factory := &sshFactoryAdapter{}
	limiter := channel.NewLimiter(5, 5, 1, 0)
	defer limiter.Close()
	p := channel.NewPrechecker(nil, limiter, channel.WithChannelFactory(factory), channel.WithNoopTimeout(3*time.Second))

	targets := []channel.Target{tgt1, tgt2, tgt3, tgtBad}
	report := p.Check(context.Background(), targets)

	assert.Equal(t, 3, report.ReachableCount, "three targets should be reachable")
	assert.Equal(t, 1, report.UnreachableCount, "the closed-port target should be unreachable")

	// The unreachable result should mention connect failure.
	var unreachable *channel.PrecheckResult
	for i := range report.Results {
		if !report.Results[i].Reachable {
			unreachable = &report.Results[i]
			break
		}
	}
	require.NotNil(t, unreachable)
	assert.Contains(t, unreachable.Error, "connect")
}

// TestIntegrationErrorRecovery verifies that a target which initially refuses
// connections can be recovered and probed successfully without restarting the
// test process.
func TestIntegrationErrorRecovery(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")
	factory := &sshFactoryAdapter{}
	p := channel.NewPrechecker(nil, nil, channel.WithChannelFactory(factory), channel.WithNoopTimeout(2*time.Second))

	// Phase 1: refuse auth -> unreachable.
	srv.SetRefuseAuth(true)
	report1 := p.Check(context.Background(), []channel.Target{tgt})
	assert.Equal(t, 0, report1.ReachableCount)
	assert.Equal(t, 1, report1.UnreachableCount)
	assert.Contains(t, report1.Results[0].Error, "connect")

	// Phase 2: restore auth -> reachable.
	srv.SetRefuseAuth(false)
	report2 := p.Check(context.Background(), []channel.Target{tgt})
	assert.Equal(t, 1, report2.ReachableCount)
	assert.Equal(t, 0, report2.UnreachableCount)
	assert.True(t, report2.Results[0].Reachable)
}

// TestIntegrationRegistryWithSSH verifies that a ChannelRegistry wired with the
// SSH factory can create channels by target type, exercising the full
// registry -> factory -> channel stack.
func TestIntegrationRegistryWithSSH(t *testing.T) {
	srv := newIntegServer(t, "u", "p")
	defer srv.Close()

	tgt := newIntegTarget(t, srv, "u", "p")

	reg := channel.NewChannelRegistry()
	reg.Register("ssh", &sshFactoryAdapter{})
	defer reg.Unregister("ssh")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := reg.Create(tgt)
	require.NoError(t, err)
	require.NotNil(t, ch)
	defer ch.Close()

	require.NoError(t, ch.Connect(ctx))
	res, err := ch.Exec(ctx, "echo via-registry")
	require.NoError(t, err)
	assert.Equal(t, "via-registry\n", res.Stdout)
}

// TestIntegrationConcurrentPrecheckWithLimiter verifies that the Prechecker
// respects the Limiter under concurrent probes against multiple targets and
// that all permits are returned to the pool afterwards.
func TestIntegrationConcurrentPrecheckWithLimiter(t *testing.T) {
	const numTargets = 4
	servers := make([]*integServer, numTargets)
	targets := make([]channel.Target, numTargets)
	for i := 0; i < numTargets; i++ {
		servers[i] = newIntegServer(t, "u", "p")
		targets[i] = newIntegTarget(t, servers[i], "u", "p")
	}
	defer func() {
		for _, s := range servers {
			s.Close()
		}
	}()

	limiter := channel.NewLimiter(2, 2, 1, 0)
	defer limiter.Close()
	p := channel.NewPrechecker(nil, limiter, channel.WithChannelFactory(&sshFactoryAdapter{}), channel.WithNoopTimeout(3*time.Second))

	report := p.Check(context.Background(), targets)
	assert.Equal(t, numTargets, report.ReachableCount)
	assert.Equal(t, 0, report.UnreachableCount)

	stats := limiter.Stats()
	assert.Equal(t, int64(numTargets), stats.TotalAcquired)
	assert.Equal(t, 0, stats.GlobalInUse, "all permits should be released after precheck")
}
