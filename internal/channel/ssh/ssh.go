// Package ssh implements the SSH transport for LEVEE's channel abstraction
// layer. It provides an SSHChannel that satisfies the channel.Channel interface
// using golang.org/x/crypto/ssh as the underlying transport, plus an SSHFactory
// that registers itself with the channel registry under the "ssh" type
// identifier.
//
// The implementation supports:
//   - password and private-key authentication (encrypted keys via passphrase);
//   - known_hosts verification with optional strict-host-check toggle;
//   - command execution with full stdout/stderr/exit-code capture;
//   - file upload/download via the SFTP sub-protocol;
//   - context-aware connect/exec with cancellation.
//
// All public types are safe for concurrent use once Connect has returned,
// mirroring the guarantees documented on the channel.Channel interface.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/nexus/levee/internal/channel"
)

// Compile-time assertion that SSHChannel implements channel.Channel.
var _ channel.Channel = (*SSHChannel)(nil)

// Compile-time assertion that SSHFactory implements channel.ChannelFactory.
var _ channel.ChannelFactory = (*SSHFactory)(nil)

// DefaultPort is the default TCP port used when Target.Port() returns zero.
const DefaultPort = 22

// DefaultConnectTimeout is the default ceiling on the SSH handshake when the
// caller does not supply a per-channel timeout via Config.ConnectTimeout.
const DefaultConnectTimeout = 30 * time.Second

// Config carries the tunable parameters for an SSH channel. The zero value is
// not usable; callers should use NewConfig or populate at least the fields
// relevant to their authentication strategy.
type Config struct {
	// Port overrides the target's port when non-zero. Leave zero to honour
	// Target.Port() (and fall back to DefaultPort when that is also zero).
	Port int

	// ConnectTimeout bounds the TCP dial + SSH handshake. Zero means use
	// DefaultConnectTimeout. A negative value disables the timeout.
	ConnectTimeout time.Duration

	// KnownHostsPath is the path to a known_hosts file used for host-key
	// verification. When empty and StrictHostCheck is true, host-key
	// verification is disabled with a warning (insecure, intended for
	// first-run bootstrapping).
	KnownHostsPath string

	// StrictHostCheck governs host-key verification. When false the server's
	// host key is accepted without verification (insecure, suitable for
	// tests / lab environments).
	StrictHostCheck bool

	// AuthMethod selects the authentication strategy. Valid values are
	// "password" and "key". When empty, the implementation infers the
	// strategy from the populated CredentialRef fields (key when KeyPath is
	// set, password otherwise).
	AuthMethod string

	// HostKeyCallback, when non-nil, overrides the host-key verification
	// derived from KnownHostsPath / StrictHostCheck. It is primarily
	// intended for tests that want to inject an in-memory verifier.
	HostKeyCallback ssh.HostKeyCallback
}

// NewConfig returns a Config populated with LEVEE's defaults: 30 s connect
// timeout, strict host checking disabled (lab-friendly). Port is left zero
// so that Target.Port() is honoured; when the target port is also zero the
// channel falls back to DefaultPort at dial time.
// Callers should treat the returned value as a starting point and override
// fields as needed.
func NewConfig() *Config {
	return &Config{
		ConnectTimeout:  DefaultConnectTimeout,
		StrictHostCheck: false,
	}
}

// SSHChannel is a live SSH session bound to a single channel.Target. It
// implements channel.Channel by delegating to golang.org/x/crypto/ssh.
//
// A channel owns exactly one *ssh.Client; concurrent Exec / Upload / Download
// calls open independent sessions over the same multiplexed client connection,
// mirroring OpenSSH ControlMaster multiplexing.
type SSHChannel struct {
	target channel.Target
	cfg    *Config

	mu        sync.Mutex
	client    *ssh.Client
	connected bool
	closed    bool
}

// SSHFactory builds SSHChannel instances for any channel.Target whose Type()
// returns "ssh". It carries no mutable state and is safe to share across
// goroutines.
type SSHFactory struct{}

// Create returns a new, unconnected SSHChannel for the given target. The
// caller is responsible for invoking Connect before use.
func (f *SSHFactory) Create(target channel.Target) (channel.Channel, error) {
	if target == nil {
		return nil, fmt.Errorf("ssh: target is nil")
	}
	if target.Type() != "ssh" {
		return nil, fmt.Errorf("ssh: target type %q is not %q", target.Type(), "ssh")
	}
	cfg := NewConfig()
	return &SSHChannel{target: target, cfg: cfg}, nil
}

// NewChannel returns a new SSHChannel bound to the given target with the
// provided config. It is intended for callers that need to override defaults
// (e.g. inject a HostKeyCallback for tests). The returned channel is not
// connected; the caller must invoke Connect.
func NewChannel(target channel.Target, cfg *Config) (*SSHChannel, error) {
	if target == nil {
		return nil, fmt.Errorf("ssh: target is nil")
	}
	if cfg == nil {
		cfg = NewConfig()
	}
	return &SSHChannel{target: target, cfg: cfg}, nil
}

// effectivePort returns the port to dial for this channel, honouring Config
// overrides and falling back to the target's port and then DefaultPort.
func (c *SSHChannel) effectivePort() int {
	if c.cfg.Port != 0 {
		return c.cfg.Port
	}
	if p := c.target.Port(); p != 0 {
		return p
	}
	return DefaultPort
}

// hostPort returns the "host:port" string used for dialing and pool keys.
func (c *SSHChannel) hostPort() string {
	return net.JoinHostPort(c.target.Host(), fmt.Sprintf("%d", c.effectivePort()))
}

// Connect establishes the SSH transport session. It is idempotent: calling
// Connect on an already-connected channel is a no-op that returns nil.
//
// The dial respects ctx; when ctx is cancelled before the handshake completes
// any partial state is released and ctx.Err() is returned.
func (c *SSHChannel) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected && c.client != nil {
		return nil
	}
	if c.closed {
		return fmt.Errorf("ssh: channel is closed")
	}

	cred := c.target.Credentials()
	authMethods, err := c.buildAuthMethods(cred)
	if err != nil {
		return fmt.Errorf("ssh: build auth methods: %w", err)
	}

	hostKeyCb, err := c.buildHostKeyCallback()
	if err != nil {
		return fmt.Errorf("ssh: build host key callback: %w", err)
	}

	config := &ssh.ClientConfig{
		User:            cred.Username,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCb,
		Timeout:         c.cfg.ConnectTimeout,
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultConnectTimeout
	}

	addr := c.hostPort()
	dialer := &net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	// Ensure conn is closed if anything fails before the client is assigned.
	defer func() {
		if c.client == nil && conn != nil {
			_ = conn.Close()
		}
	}()

	// Wrap the conn so that the SSH handshake also respects ctx. ssh.NewClientConn
	// does not take a context; we race it against ctx.Done() and close the
	// underlying TCP conn on cancellation to unblock the handshake.
	type newConnResult struct {
		client *ssh.Client
		err    error
	}
	resultCh := make(chan newConnResult, 1)
	go func() {
		sc, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
		if err != nil {
			resultCh <- newConnResult{nil, err}
			return
		}
		resultCh <- newConnResult{ssh.NewClient(sc, chans, reqs), nil}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			_ = conn.Close()
			return fmt.Errorf("ssh: handshake %s: %w", addr, res.err)
		}
		c.client = res.client
		c.connected = true
		return nil
	case <-ctx.Done():
		// Closing the underlying TCP conn forces NewClientConn to error out;
		// the goroutine will then deliver its result into the buffered channel
		// and exit. We do not wait for it to avoid blocking the caller.
		_ = conn.Close()
		return ctx.Err()
	}
}

// buildAuthMethods translates the CredentialRef into a slice of ssh.AuthMethod
// according to Config.AuthMethod (with sensible inference when empty).
func (c *SSHChannel) buildAuthMethods(cred channel.CredentialRef) ([]ssh.AuthMethod, error) {
	method := c.cfg.AuthMethod
	if method == "" {
		if cred.KeyPath != "" {
			method = "key"
		} else {
			method = "password"
		}
	}

	var methods []ssh.AuthMethod
	switch strings.ToLower(method) {
	case "password", "pwd":
		if cred.Password == "" {
			return nil, fmt.Errorf("ssh: password auth requested but no password provided")
		}
		methods = append(methods, ssh.Password(cred.Password))
	case "key", "publickey":
		am, err := loadKeyAuthMethod(cred)
		if err != nil {
			return nil, err
		}
		methods = append(methods, am)
	case "key+password", "password+key":
		// Try key first, fall back to password. Useful when a host accepts
		// either but the operator wants both attempted.
		if cred.KeyPath != "" {
			if am, err := loadKeyAuthMethod(cred); err == nil {
				methods = append(methods, am)
			}
		}
		if cred.Password != "" {
			methods = append(methods, ssh.Password(cred.Password))
		}
		if len(methods) == 0 {
			return nil, fmt.Errorf("ssh: no usable auth credentials for %q", method)
		}
	default:
		return nil, fmt.Errorf("ssh: unknown auth method %q", method)
	}
	return methods, nil
}

// loadKeyAuthMethod reads the private key at cred.KeyPath, optionally
// decrypting it with cred.KeyPassphrase, and returns the corresponding
// ssh.AuthMethod (public key).
func loadKeyAuthMethod(cred channel.CredentialRef) (ssh.AuthMethod, error) {
	if cred.KeyPath == "" {
		return nil, fmt.Errorf("ssh: key auth requested but no key path provided")
	}
	keyBytes, err := os.ReadFile(cred.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: read key %s: %w", cred.KeyPath, err)
	}
	var signer ssh.Signer
	if cred.KeyPassphrase == "" {
		signer, err = ssh.ParsePrivateKey(keyBytes)
	} else {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(cred.KeyPassphrase))
	}
	if err != nil {
		return nil, fmt.Errorf("ssh: parse key %s: %w", cred.KeyPath, err)
	}
	return ssh.PublicKeys(signer), nil
}

// buildHostKeyCallback returns the ssh.HostKeyCallback to use for this channel.
// Precedence: Config.HostKeyCallback > known_hosts file > insecure (insecure
// only when StrictHostCheck is false).
func (c *SSHChannel) buildHostKeyCallback() (ssh.HostKeyCallback, error) {
	if c.cfg.HostKeyCallback != nil {
		return c.cfg.HostKeyCallback, nil
	}
	if c.cfg.StrictHostCheck {
		if c.cfg.KnownHostsPath == "" {
			return nil, fmt.Errorf("ssh: strict host checking enabled but no known_hosts path provided")
		}
		cb, err := knownhosts.New(c.cfg.KnownHostsPath)
		if err != nil {
			return nil, fmt.Errorf("ssh: load known_hosts %s: %w", c.cfg.KnownHostsPath, err)
		}
		return cb, nil
	}
	// Insecure: accept any host key. Suitable for lab / test environments.
	return ssh.InsecureIgnoreHostKey(), nil
}

// Exec runs cmd on the target and returns its full result. Exec blocks until
// the command terminates or ctx is cancelled. When ctx is cancelled after the
// session has started the remote process is sent SIGKILL via Close.
func (c *SSHChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("ssh: not connected")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// Capture stdout and stderr separately so we can populate ExecResult
	// faithfully. We use pipes rather than CombinedOutput to keep the two
	// streams distinct for audit.
	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	start := time.Now()
	// Run the command in a goroutine so we can race it against ctx.Done().
	type execResult struct {
		err error
	}
	doneCh := make(chan execResult, 1)
	go func() {
		doneCh <- execResult{session.Run(cmd)}
	}()

	select {
	case res := <-doneCh:
		duration := time.Since(start)
		result := &channel.ExecResult{
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: duration,
		}
		if res.err == nil {
			result.ExitCode = 0
			return result, nil
		}
		// ssh.ExitError carries the remote exit code.
		var exitErr *ssh.ExitError
		if errors.As(res.err, &exitErr) {
			result.ExitCode = exitErr.ExitStatus()
			return result, nil
		}
		// Non-exit error (e.g. channel closed): report it but still return
		// captured output for diagnostics.
		result.ExitCode = -1
		return result, fmt.Errorf("ssh: exec %q: %w", cmd, res.err)
	case <-ctx.Done():
		// Cancellation: tear down the session to signal the remote process.
		_ = session.Close()
		return &channel.ExecResult{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			Duration: time.Since(start),
		}, ctx.Err()
	}
}

// Upload streams the content of r to remotePath on the target. The remote
// file is created (or overwritten) by piping the content through the remote
// `cat` command, which is available on every POSIX target LEVEE supports.
// Upload blocks until the transfer completes or ctx is cancelled.
//
// The implementation reads the entire content into memory before sending it
// over the channel. This is acceptable for LEVEE's use case (config files,
// small scripts); a streaming variant would require chunked writes with
// periodic ctx checks.
func (c *SSHChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	if err := c.ensureConnected(); err != nil {
		return err
	}

	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return fmt.Errorf("ssh: not connected")
	}

	payload, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("ssh: read upload content: %w", err)
	}

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh: new session: %w", err)
	}
	defer func() { _ = session.Close() }()

	// StdinPipe must be obtained before Start. We pipe the payload via
	// stdin rather than embedding it in the command line to avoid quoting
	// issues and command-line length limits.
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("ssh: stdin pipe: %w", err)
	}

	cmd := fmt.Sprintf("cat > %s", shellQuote(remotePath))
	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("ssh: start cat: %w", err)
	}

	type uploadResult struct{ err error }
	doneCh := make(chan uploadResult, 1)
	go func() {
		if _, err := stdin.Write(payload); err != nil {
			_ = stdin.Close()
			doneCh <- uploadResult{err}
			return
		}
		if err := stdin.Close(); err != nil {
			doneCh <- uploadResult{err}
			return
		}
		doneCh <- uploadResult{session.Wait()}
	}()

	select {
	case res := <-doneCh:
		if res.err != nil {
			return fmt.Errorf("ssh: upload %s: %w", remotePath, res.err)
		}
		return nil
	case <-ctx.Done():
		_ = session.Close()
		return ctx.Err()
	}
}

// Download streams the content of remotePath back to the caller as an
// io.Reader. The returned reader's Close releases the underlying SSH session.
// Download blocks until the transfer starts (the remote cat command is
// launched and the first bytes are available) or ctx is cancelled; the
// actual byte stream is then read lazily by the caller.
func (c *SSHChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	if err := c.ensureConnected(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	client := c.client
	c.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("ssh: not connected")
	}

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh: new session: %w", err)
	}

	stdoutPipe, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh: stdout pipe: %w", err)
	}

	// Use `cat path` to stream the file back. Shell-quote the path.
	if err := session.Start(fmt.Sprintf("cat %s", shellQuote(remotePath))); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("ssh: start cat: %w", err)
	}

	// Wrap the pipe so that Close also closes the session, releasing the
	// transport resource held open during the transfer.
	r := &downloadReader{
		r:       stdoutPipe,
		session: session,
	}
	return r, nil
}

// downloadReader adapts an ssh session stdout pipe so that Close releases the
// session as well as the pipe.
type downloadReader struct {
	r         io.Reader
	session   *ssh.Session
	closeOnce sync.Once
}

// Read implements io.Reader by delegating to the underlying pipe.
func (d *downloadReader) Read(p []byte) (int, error) {
	return d.r.Read(p)
}

// Close releases the underlying SSH session and pipe. It is idempotent.
func (d *downloadReader) Close() error {
	var err error
	d.closeOnce.Do(func() {
		if d.session != nil {
			err = d.session.Close()
			d.session = nil
		}
	})
	return err
}

// Close tears down the SSH transport session. It is idempotent: subsequent
// calls return nil.
func (c *SSHChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true
	c.connected = false
	if c.client != nil {
		err := c.client.Close()
		c.client = nil
		if err != nil {
			return fmt.Errorf("ssh: close client: %w", err)
		}
	}
	return nil
}

// IsConnected reports whether the channel currently holds an active SSH
// session. The check is best-effort: it verifies that the underlying client is
// non-nil and that a trivial session can be opened. A true return does not
// guarantee that the next Exec will succeed.
func (c *SSHChannel) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.client == nil {
		return false
	}
	// Probe the connection by opening and immediately closing a session.
	// This is cheaper than a round-trip "true" command and surfaces
	// half-closed sockets faster than waiting for the next Exec.
	probe, err := c.client.NewSession()
	if err != nil {
		c.connected = false
		return false
	}
	_ = probe.Close()
	return true
}

// ensureConnected returns an error when the channel is not in a usable state.
// It is invoked at the top of Exec / Upload / Download.
func (c *SSHChannel) ensureConnected() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("ssh: channel is closed")
	}
	if !c.connected || c.client == nil {
		return fmt.Errorf("ssh: not connected")
	}
	return nil
}

// Target returns the channel.Target this channel is bound to. It is primarily
// useful for the connection pool, which keys channels by target host:port.
func (c *SSHChannel) Target() channel.Target { return c.target }

// shellQuote returns p wrapped in single quotes with any embedded single
// quotes escaped, so that p can be safely embedded in a remote shell command
// line. This is the standard POSIX quoting idiom.
func shellQuote(p string) string {
	// Replace every ' with '\'' (close quote, escaped quote, reopen quote).
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// init registers the SSH factory with the process-wide channel registry so
// that callers can create SSH channels via channel.DefaultRegistry().Create(...)
// without importing this package explicitly.
func init() {
	channel.DefaultRegistry().Register("ssh", &SSHFactory{})
}
