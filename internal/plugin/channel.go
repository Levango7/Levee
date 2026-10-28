// channel.go is the execution half of the channel plugin type: it adapts an
// enabled registry record (TypeChannel) into a channel.ChannelFactory plus the
// channel.Channel that factory builds, so an inventory target whose
// `channel_type` names the plugin dials through it exactly like a built-in
// transport.
//
// Unlike the other consumption points this one is a SESSION, not a one-shot:
// the plugin process is started at Connect, served requests (exec / collect)
// for the life of the run's channel, and — since D-1c — returned to a
// per-plugin pool instead of being killed at Close, so a later run reuses the
// dial (pool.go holds the sharing rules and the bounds). See session.go for the
// process half and its invariants (serialised requests; a failed request kills
// the session rather than leaving the stream desynchronised).
//
// The channel type a plugin provides is its registry name: one plugin, one
// transport type. Targets opt in by declaring that name in `channel_type`.
package plugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// PluginChannelFactory implements channel.ChannelFactory for one enabled
// channel plugin. The zero value is not usable; construct with
// NewPluginChannelFactory.
type PluginChannelFactory struct {
	name       string
	binaryPath string
	configYAML string

	// pool is process-wide for this plugin: every channel this factory builds
	// shares it, and it outlives individual runs. The reaper goroutine stops
	// when the factory's construction context (serve's lifecycle) ends.
	pool *sessionPool
}

// NewPluginChannelFactory builds the factory and performs one meta handshake,
// the same boot-time usability question the other tiers ask: an enabled
// channel that cannot identify itself is unusable, and the caller refuses the
// boot with this error rather than discovering it on the first dial.
//
// ctx is also the pool's lifetime: when serve's context ends, the reaper stops
// and every idle pooled session is torn down.
func NewPluginChannelFactory(ctx context.Context, rec *RegistryRecord) (*PluginChannelFactory, error) {
	if rec == nil {
		return nil, fmt.Errorf("plugin: channel: nil registry record")
	}
	if rec.Type != TypeChannel {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record is %q, want %q", ErrInvalidPluginType, rec.Type, TypeChannel))
	}
	if rec.State != StateEnabled {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record state is %q", ErrPluginNotEnabled, rec.State))
	}

	responses, err := Probe(ctx, rec.BinaryPath, []HostRequest{{Action: "meta"}})
	if err != nil {
		return nil, wrapError(rec.Name, fmt.Errorf("meta handshake: %w", err))
	}
	if len(responses) != 1 || !responses[0].OK {
		if len(responses) == 1 {
			return nil, wrapError(rec.Name, fmt.Errorf("meta refused: %s", responses[0].Error))
		}
		return nil, wrapError(rec.Name, fmt.Errorf("meta handshake returned %d responses, want 1", len(responses)))
	}

	f := &PluginChannelFactory{
		name:       rec.Name,
		binaryPath: rec.BinaryPath,
		configYAML: rec.ConfigYAML,
		pool:       newSessionPool(),
	}
	go f.pool.run(ctx, poolSweepInterval)
	return f, nil
}

// Name returns the transport type this factory serves — the plugin's registry
// name, which is what an inventory row declares in `channel_type`.
func (f *PluginChannelFactory) Name() string { return f.name }

// Create returns a new, unconnected PluginChannel bound to target. The caller
// connects it (channelFor does so immediately after creation).
func (f *PluginChannelFactory) Create(target channel.Target) (channel.Channel, error) {
	if target == nil {
		return nil, wrapError(f.name, fmt.Errorf("channel: nil target"))
	}
	return &PluginChannel{
		factory: f,
		target:  target,
		key: poolKey{
			plugin: f.name,
			host:   target.Host(),
			port:   target.Port(),
			typ:    target.Type(),
			credFP: credentialFingerprint(target.Credentials()),
		},
	}, nil
}

// PluginChannel is one transport session implemented by a resident plugin
// process. It implements channel.Channel; the zero value is not usable.
//
// The channel holds a LEASE on a pooled session, not the session's lifetime:
// Close gives the lease back (the pool keeps the session for the next run),
// while a dead session is dropped by the pool on sight.
type PluginChannel struct {
	factory *PluginChannelFactory
	target  channel.Target
	key     poolKey

	// mu guards sess, the lease handle. Request serialisation lives on the
	// session itself; this lock only protects the handle against Connect and
	// Close racing each other.
	mu   sync.Mutex
	sess poolSession
}

func (c *PluginChannel) session() poolSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// Connect acquires a session: a pooled one when the same (plugin, target,
// credential) is already live, otherwise a fresh dial handed to the plugin
// (process start, init with install-time config, the plugin's own connect).
// It is idempotent by contract — connecting an already-connected channel
// returns nil.
func (c *PluginChannel) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sess != nil {
		if c.sess.Alive() {
			return nil
		}
		// A previous session died (plugin crash, cancelled request). Hand the
		// dead lease back so the pool drops and reaps it, then re-dial.
		c.factory.pool.release(c.key, c.sess)
		c.sess = nil
	}

	if sess, ok := c.factory.pool.acquire(c.key); ok {
		c.sess = sess
		return nil
	}

	connectCtx, cancel := context.WithTimeout(ctx, sessionConnectDeadline)
	defer cancel()

	sess, err := startPluginSession(connectCtx, c.factory.name, c.factory.binaryPath, c.factory.configYAML)
	if err != nil {
		return err
	}
	resp, err := sess.Request(connectCtx, HostRequest{Action: "connect", Params: channelConnectParams(c.target)})
	if err != nil {
		sess.kill()
		return wrapError(c.factory.name, fmt.Errorf("connect: %w", err))
	}
	if !resp.OK {
		sess.kill()
		return wrapError(c.factory.name, fmt.Errorf("connect refused: %s", resp.Error))
	}

	published, ok := c.factory.pool.publish(c.key, sess)
	if !ok {
		// The pool was shut down while this dial was in flight; publish has
		// already torn the session down.
		return wrapError(c.factory.name, fmt.Errorf("connect: plugin session pool is shutting down"))
	}
	c.sess = published
	return nil
}

// Exec runs one command through the plugin session and maps the answer onto
// channel.ExecResult — the four fields are audit evidence, so an answer that
// carries no exec payload is refused rather than read as a silent success.
//
// The caller's ctx bounds the request: when it ends, the session is killed
// (the stream cannot be resynchronised) and ctx.Err() surfaces verbatim, which
// makes the channel report IsConnected false and the next use re-dial.
func (c *PluginChannel) Exec(ctx context.Context, cmd string) (*channel.ExecResult, error) {
	sess := c.session()
	if sess == nil || !sess.Alive() {
		return nil, wrapError(c.factory.name, fmt.Errorf("channel: not connected"))
	}

	resp, err := sess.Request(ctx, HostRequest{Action: "exec", Params: map[string]any{"cmd": cmd}})
	if err != nil {
		// ctx errors surface verbatim (the Channel contract asks for that);
		// session errors already name the plugin.
		return &channel.ExecResult{ExitCode: -1}, err
	}
	if !resp.OK {
		// -1, never a faked 0: the command did not run, and the audit trail
		// must not read that as success.
		return &channel.ExecResult{ExitCode: -1}, wrapError(c.factory.name, fmt.Errorf("exec refused: %s", resp.Error))
	}
	if resp.Exec == nil {
		return &channel.ExecResult{ExitCode: -1}, wrapError(c.factory.name, fmt.Errorf("exec returned ok with no result; an empty success is not a verdict"))
	}
	return &channel.ExecResult{
		ExitCode: resp.Exec.ExitCode,
		Stdout:   resp.Exec.Stdout,
		Stderr:   resp.Exec.Stderr,
		Duration: time.Duration(resp.Exec.Duration) * time.Millisecond,
	}, nil
}

// Upload pushes one file to the target through the plugin's upload verb: the
// content is base64-encoded into the request line and handed over whole.
//
// The payload is bounded by maxTransferPayloadBytes (the JSON-line frame, not
// a policy choice) — the host refuses an oversize transfer BEFORE sending
// rather than desynchronising the stream mid-line. Larger files must chunk or
// use a built-in transport.
func (c *PluginChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	sess := c.session()
	if sess == nil || !sess.Alive() {
		return wrapError(c.factory.name, fmt.Errorf("channel: not connected"))
	}

	data, err := readBounded(content, maxTransferPayloadBytes)
	if err != nil {
		return wrapError(c.factory.name, err)
	}

	resp, err := sess.Request(ctx, HostRequest{Action: "upload", Params: map[string]any{
		"path": remotePath,
		"data": base64.StdEncoding.EncodeToString(data),
	}})
	if err != nil {
		return err
	}
	if !resp.OK {
		return wrapError(c.factory.name, fmt.Errorf("upload refused: %s", resp.Error))
	}
	return nil
}

// readBounded reads r fully, failing with a named error when it exceeds cap —
// the +1 byte is what turns "exactly at the cap" from a guess into a check.
func readBounded(r io.Reader, cap int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, fmt.Errorf("read upload content: %w", err)
	}
	if int64(len(data)) > cap {
		return nil, fmt.Errorf("upload content is %d bytes; a plugin channel carries at most %d per transfer (the JSON-line frame) — chunk it or use a built-in transport", len(data), cap)
	}
	return data, nil
}

// Download retrieves a path from the target through the plugin's collect verb
// and hands the caller a reader over the decoded bytes. The payload bound is
// the same frame as upload's; a plugin that answers over it fails the request
// here with a named error instead of a scanner tear-down deeper in the pipe.
func (c *PluginChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	sess := c.session()
	if sess == nil || !sess.Alive() {
		return nil, wrapError(c.factory.name, fmt.Errorf("channel: not connected"))
	}

	resp, err := sess.Request(ctx, HostRequest{Action: "collect", Params: map[string]any{"path": remotePath}})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, wrapError(c.factory.name, fmt.Errorf("collect refused: %s", resp.Error))
	}
	if resp.Collect == nil {
		return nil, wrapError(c.factory.name, fmt.Errorf("collect returned ok with no payload; an empty success is not a verdict"))
	}
	if len(resp.Collect.Data) > base64.StdEncoding.EncodedLen(int(maxTransferPayloadBytes))+64 {
		return nil, wrapError(c.factory.name, fmt.Errorf("collect payload is %d base64 chars; a plugin channel carries at most %d raw bytes per transfer (the JSON-line frame)", len(resp.Collect.Data), maxTransferPayloadBytes))
	}
	data, err := base64.StdEncoding.DecodeString(resp.Collect.Data)
	if err != nil {
		return nil, wrapError(c.factory.name, fmt.Errorf("collect payload is not base64: %w", err))
	}
	return bytes.NewReader(data), nil
}

// Close returns the lease on the pooled session. It is idempotent by contract
// and never fails: the run is done with the channel either way.
//
// This is a deliberate departure from "Close tears down the transport session":
// for a runtime plugin the session outlives the run by design (D-1c), so Close
// ends YOUR use of it and the pool owns the lifetime — bounded by the idle TTL,
// the max age and the pool's shutdown. The run-visible contract is unchanged:
// after Close the channel is disconnected and refuses work, and a dead session
// is never handed to the next run.
func (c *PluginChannel) Close() error {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()

	if sess == nil {
		return nil
	}
	c.factory.pool.release(c.key, sess)
	return nil
}

// IsConnected implements channel.Channel: false guarantees Connect must be
// called before any other method (channelFor re-dials on it).
func (c *PluginChannel) IsConnected() bool {
	sess := c.session()
	return sess != nil && sess.Alive()
}

// channelConnectParams builds the addressing a channel plugin receives.
//
// Plaintext credentials are deliberately absent. The declared ChannelPlugin
// contract says the host passes an opaque credential reference and the plugin
// resolves the secret through its own mechanism; for a runtime plugin that
// mechanism is its install-time config (config.yaml, stored in the registry),
// which is also what the transport-native-credentials rule in
// docs/channel-plugins.md asks for. The host therefore sends the host/port/
// type and — when the inventory row names one — the reference's NAME, never
// the expanded CredentialRef.
func channelConnectParams(target channel.Target) map[string]any {
	params := map[string]any{
		"host": target.Host(),
		"port": target.Port(),
		"type": target.Type(),
	}
	if namer, ok := target.(channel.CredentialRefNamer); ok {
		if ref := namer.CredentialRefName(); ref != "" {
			params["credential_ref"] = ref
		}
	}
	return params
}
