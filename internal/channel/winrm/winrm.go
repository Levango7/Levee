// Package winrm implements the WinRM transport for the LEVEE channel
// abstraction layer.
//
// WinRM (Windows Remote Management) is the Microsoft protocol for remote
// management of Windows hosts. It is HTTP(S)-based and uses SOAP/WS-Management
// envelopes. Unlike SSH, WinRM has no multiplexing: each command runs in its
// own shell and the protocol is effectively single-command-per-shell.
//
// This package wraps github.com/masterzen/winrm and exposes it through the
// channel.Channel interface so that the upper orchestration layers can drive
// Windows targets uniformly alongside SSH / Agent / API targets.
//
// Design notes (see docs/levee-design.md §4.1.3):
//   - Connection pool + single-connection-single-command strategy.
//   - A WinRMChannel owns one winrm.Client (an HTTP client + credentials).
//   - Each Exec creates a transient shell inside the client, runs the command,
//     and closes the shell. The underlying HTTP connection is reused across
//     Exec calls; the shell is not.
//   - Close is a logical no-op that drops the cached client; WinRM is stateless
//     at the transport level so there is no persistent session to tear down.
package winrm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/masterzen/winrm"

	"github.com/nexus/levee/internal/channel"
)

// --- constants --------------------------------------------------------------

const (
	// DefaultHTTPPort is the default WinRM HTTP port (5985).
	DefaultHTTPPort = 5985
	// DefaultHTTPSPort is the default WinRM HTTPS port (5986).
	DefaultHTTPSPort = 5986
	// DefaultTimeout is the default connection timeout when Config.Timeout is
	// zero.
	DefaultTimeout = 30 * time.Second
	// DefaultTransport is the default auth transport (negotiate).
	DefaultTransport = "negotiate"

	// TransportNegotiate uses SPNEGO/Kerberos-or-NTLM negotiation.
	TransportNegotiate = "negotiate"
	// TransportNTLM forces NTLMv2 authentication.
	TransportNTLM = "ntlm"

	// uploadChunkSize bounds the base64 payload sent in a single PowerShell
	// invocation. Windows has a ~32 KB command-line limit; we stay well under
	// it so the encoded blob plus the PowerShell wrapper fit comfortably.
	uploadChunkSize = 24 * 1024 // 24 KiB raw -> ~32 KiB base64
)

// --- Config -----------------------------------------------------------------

// Config holds the configuration for a WinRM channel. The zero value is not
// usable; at minimum Port or HTTPS should be set (or rely on the defaults
// applied in resolvePort).
type Config struct {
	// Port overrides the target port. 0 means "derive from HTTPS flag"
	// (5985 for HTTP, 5986 for HTTPS).
	Port int

	// HTTPS selects HTTPS transport. When true and Port is 0, the port
	// defaults to 5986.
	HTTPS bool

	// Insecure skips TLS certificate verification when HTTPS is true.
	// Equivalent to curl's -k. Use only in lab / pre-prod.
	Insecure bool

	// Transport selects the auth transport: "negotiate" (default) or "ntlm".
	// Unknown values cause Create to return an error.
	Transport string

	// Timeout is the underlying TCP/HTTP dial timeout. 0 means DefaultTimeout.
	Timeout time.Duration

	// OperationTimeout is the WinRM operation timeout sent in the SOAP
	// envelope (the server-side max run time for a single command). 0 means
	// 60 seconds (the masterzen/winrm default).
	OperationTimeout time.Duration
}

// withDefaults returns a copy of c with zero values replaced by defaults.
func (c Config) withDefaults() Config {
	out := c
	if out.Transport == "" {
		out.Transport = DefaultTransport
	} else {
		out.Transport = strings.ToLower(out.Transport)
	}
	if out.Timeout == 0 {
		out.Timeout = DefaultTimeout
	}
	return out
}

// resolvePort returns the effective port given the config and a target port
// hint. Precedence: target.Port() > Config.Port > default-by-HTTPS.
func (c Config) resolvePort(targetPort int) int {
	if targetPort > 0 {
		return targetPort
	}
	if c.Port > 0 {
		return c.Port
	}
	if c.HTTPS {
		return DefaultHTTPSPort
	}
	return DefaultHTTPPort
}

// --- WinRMChannel -----------------------------------------------------------

// WinRMChannel implements channel.Channel over the WinRM protocol.
//
// Privilege boundary: every command runs as the account that authenticated
// the WinRM session — there is no runas / elevation wrapping in the MVP.
// Use an administrator account on the Windows target when workflows need
// privileged operations; non-admin accounts simply fail with access-denied
// errors from the remote side.
//
// A WinRMChannel is safe for concurrent use once connected: each Exec takes a
// short-lived lock only to read the cached client, and the underlying
// winrm.Client creates a fresh shell per Run so two concurrent Exec calls do
// not interfere. The connection pool (see pool.go) is the recommended way to
// bound concurrency per target.
type WinRMChannel struct {
	target channel.Target
	cfg    Config

	mu        sync.RWMutex
	client    *winrm.Client
	connected bool
}

// compile-time interface check.
var _ channel.Channel = (*WinRMChannel)(nil)

// newChannel constructs an unconnected WinRMChannel. It validates the target
// and resolves configuration defaults but does not perform any I/O.
func newChannel(target channel.Target, cfg Config) (*WinRMChannel, error) {
	if target == nil {
		return nil, errors.New("winrm: target is nil")
	}
	if target.Host() == "" {
		return nil, errors.New("winrm: target host is empty")
	}
	cred := target.Credentials()
	if cred.Username == "" {
		return nil, errors.New("winrm: username is required for WinRM")
	}

	switch strings.ToLower(cfg.Transport) {
	case "", TransportNegotiate, TransportNTLM:
		// ok
	default:
		return nil, fmt.Errorf("winrm: unsupported transport %q", cfg.Transport)
	}

	return &WinRMChannel{
		target: target,
		cfg:    cfg.withDefaults(),
	}, nil
}

// buildEndpoint assembles the winrm.Endpoint from the channel config + target.
func (c *WinRMChannel) buildEndpoint() *winrm.Endpoint {
	return &winrm.Endpoint{
		Host:     c.target.Host(),
		Port:     c.cfg.resolvePort(c.target.Port()),
		HTTPS:    c.cfg.HTTPS,
		Insecure: c.cfg.Insecure,
		Timeout:  c.cfg.Timeout,
	}
}

// buildClient creates a new winrm.Client with the configured transport.
func (c *WinRMChannel) buildClient() (*winrm.Client, error) {
	endpoint := c.buildEndpoint()
	cred := c.target.Credentials()

	timeoutStr := "PT60S"
	if c.cfg.OperationTimeout > 0 {
		timeoutStr = formatISO8601(c.cfg.OperationTimeout)
	}
	params := winrm.NewParameters(timeoutStr, "en-US", 153600)

	switch c.cfg.Transport {
	case TransportNTLM:
		params.TransportDecorator = func() winrm.Transporter {
			return &winrm.ClientNTLM{}
		}
	case TransportNegotiate, "":
		// masterzen/winrm defaults to negotiate; no decorator needed.
	default:
		return nil, fmt.Errorf("winrm: unsupported transport %q", c.cfg.Transport)
	}

	client, err := winrm.NewClientWithParameters(endpoint, cred.Username, cred.Password, params)
	if err != nil {
		return nil, fmt.Errorf("winrm: create client: %w", err)
	}
	return client, nil
}

// formatISO8601 converts a duration to an ISO 8601 duration string like
// "PT120S". Sub-second precision is truncated; durations <= 0 yield "PT60S".
func formatISO8601(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs <= 0 {
		return "PT60S"
	}
	return fmt.Sprintf("PT%dS", secs)
}

// psQuote escapes a string for interpolation into a single-quoted PowerShell
// literal, where a single quote is escaped by doubling it. Workflow-supplied
// remote paths are untrusted input: without this escaping a path containing
// a quote would terminate the literal and execute arbitrary script on the
// Windows target.
func psQuote(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// Connect establishes the WinRM session by constructing the underlying
// winrm.Client. It is idempotent: calling Connect on an already-connected
// channel is a no-op.
//
// WinRM has no handshake step separate from the first request, so Connect
// only builds the client. Actual network I/O happens on the first Exec /
// Upload / Download. We nonetheless honour ctx.Err() so callers can bail early.
func (c *WinRMChannel) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		return nil
	}

	client, err := c.buildClient()
	if err != nil {
		return err
	}
	c.client = client
	c.connected = true
	return nil
}

// Exec runs a command on the target and returns its full result. The command
// is executed non-interactively in a transient WinRM shell.
//
// Exec respects ctx: if ctx is cancelled before the command completes, the
// underlying RunWithContext returns and the partial result is returned
// alongside ctx.Err().
func (c *WinRMChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.RLock()
	if !c.connected || c.client == nil {
		c.mu.RUnlock()
		return nil, errors.New("winrm: channel not connected")
	}
	client := c.client
	c.mu.RUnlock()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	exitCode, runErr := client.RunWithContext(ctx, cmd, &stdout, &stderr)
	duration := time.Since(start)

	result := &channel.ExecResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: duration,
	}
	if runErr != nil {
		return result, fmt.Errorf("winrm: exec %q: %w", cmd, runErr)
	}
	return result, nil
}

// Upload streams the content of r to remotePath on the target. The content is
// base64-encoded and written via PowerShell in chunks of uploadChunkSize raw
// bytes, which keeps each PowerShell invocation well under the Windows
// command-line length limit.
func (c *WinRMChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.RLock()
	if !c.connected || c.client == nil {
		c.mu.RUnlock()
		return errors.New("winrm: channel not connected")
	}
	client := c.client
	c.mu.RUnlock()

	// Read the full payload. For very large files a streaming approach would
	// be better, but WinRM upload is inherently slow (base64 over SOAP) and
	// LEVEE uses it only for small artifacts (scripts, configs).
	data, err := io.ReadAll(content)
	if err != nil {
		return fmt.Errorf("winrm: read upload content: %w", err)
	}

	// Write the file in chunks: first chunk creates the file, subsequent
	// chunks append. This avoids a single oversized PowerShell command.
	first := true
	for offset := 0; offset < len(data); offset += uploadChunkSize {
		if err := ctx.Err(); err != nil {
			return err
		}

		end := offset + uploadChunkSize
		if end > len(data) {
			end = len(data)
		}
		encoded := base64.StdEncoding.EncodeToString(data[offset:end])
		quoted := psQuote(remotePath)

		var ps string
		if first {
			ps = fmt.Sprintf(
				`$b=[Convert]::FromBase64String('%s');[IO.File]::WriteAllBytes('%s',$b)`,
				encoded, quoted,
			)
			first = false
		} else {
			ps = fmt.Sprintf(
				`$b=[Convert]::FromBase64String('%s');$s=[IO.File]::Open('%s','Append');$s.Write($b);$s.Close()`,
				encoded, quoted,
			)
		}

		stdout, stderr, exit, runErr := client.RunPSWithContext(ctx, ps)
		if runErr != nil {
			return fmt.Errorf("winrm: upload chunk at offset %d: %w", offset, runErr)
		}
		if exit != 0 {
			return fmt.Errorf("winrm: upload chunk at offset %d failed (exit %d): %s",
				offset, exit, strings.TrimSpace(stderr+stdout))
		}
	}
	return nil
}

// Download streams the content of remotePath back to the caller as an
// io.Reader. The file is read via PowerShell, base64-encoded, and decoded
// locally. The returned reader is an in-memory bytes.Reader whose Close is a
// no-op.
//
// For very large files this loads the entire content into memory; WinRM
// download is intended for small artifacts only.
func (c *WinRMChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.mu.RLock()
	if !c.connected || c.client == nil {
		c.mu.RUnlock()
		return nil, errors.New("winrm: channel not connected")
	}
	client := c.client
	c.mu.RUnlock()

	ps := fmt.Sprintf(
		`$b=[IO.File]::ReadAllBytes('%s');[Convert]::ToBase64String($b)`,
		psQuote(remotePath),
	)

	stdout, stderr, exit, runErr := client.RunPSWithContext(ctx, ps)
	if runErr != nil {
		return nil, fmt.Errorf("winrm: download %q: %w", remotePath, runErr)
	}
	if exit != 0 {
		return nil, fmt.Errorf("winrm: download %q failed (exit %d): %s",
			remotePath, exit, strings.TrimSpace(stderr))
	}

	// PowerShell emits a UTF-8 BOM + trailing CRLF; strip both.
	out := strings.TrimSpace(stdout)
	out = strings.TrimPrefix(out, "\uFEFF")

	data, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return nil, fmt.Errorf("winrm: decode downloaded base64: %w", err)
	}
	return bytes.NewReader(data), nil
}

// Close releases the cached client. WinRM is stateless at the transport level
// so there is no persistent session to tear down; Close is effectively a
// logical reset that makes the channel require Connect before the next use.
// Close is idempotent.
func (c *WinRMChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = nil
	c.connected = false
	return nil
}

// IsConnected reports whether the channel currently holds a client. The result
// is best-effort: a true value does not guarantee the next Exec will succeed
// (the peer may have become unreachable), but a false value guarantees that
// Connect must be called first.
func (c *WinRMChannel) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Target returns the target this channel was built for. It is intended for
// pool bookkeeping and diagnostics; callers must not mutate the target.
func (c *WinRMChannel) Target() channel.Target {
	return c.target
}

// --- WinRMFactory -----------------------------------------------------------

// WinRMFactory implements channel.ChannelFactory. It carries the transport
// defaults (port, HTTPS, timeout, auth) that are applied to every channel it
// creates; per-channel overrides come from the Target.
type WinRMFactory struct {
	cfg Config
}

// compile-time interface check.
var _ channel.ChannelFactory = (*WinRMFactory)(nil)

// NewFactory returns a WinRMFactory that applies cfg to every channel it
// creates. The config is normalized via withDefaults on first use.
func NewFactory(cfg Config) *WinRMFactory {
	return &WinRMFactory{cfg: cfg}
}

// Create returns a new, unconnected WinRMChannel for the given target. The
// caller is responsible for invoking Connect before use.
func (f *WinRMFactory) Create(target channel.Target) (channel.Channel, error) {
	return newChannel(target, f.cfg)
}

// init registers the WinRMFactory with the default channel registry using the
// "winrm" type identifier. This allows the orchestrator to create WinRM
// channels without importing this package directly; importing for its side
// effect is enough.
func init() {
	channel.DefaultRegistry().Register("winrm", NewFactory(Config{}))
}
