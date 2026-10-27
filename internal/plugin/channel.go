// channel.go is the execution half of the channel plugin type: it adapts an
// enabled registry record (TypeChannel) into a channel.ChannelFactory plus the
// channel.Channel that factory builds, so an inventory target whose
// `channel_type` names the plugin dials through it exactly like a built-in
// transport.
//
// Unlike the other consumption points this one is a SESSION, not a one-shot:
// the plugin process is started at Connect, served requests (exec / collect)
// for the life of the run's channel, and torn down at Close. See session.go
// for the process half and its two invariants (serialised requests; a failed
// request kills the session rather than leaving the stream desynchronised).
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
}

// NewPluginChannelFactory builds the factory and performs one meta handshake,
// the same boot-time usability question the other tiers ask: an enabled
// channel that cannot identify itself is unusable, and the caller refuses the
// boot with this error rather than discovering it on the first dial.
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

	return &PluginChannelFactory{
		name:       rec.Name,
		binaryPath: rec.BinaryPath,
		configYAML: rec.ConfigYAML,
	}, nil
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
		name:       f.name,
		binaryPath: f.binaryPath,
		configYAML: f.configYAML,
		target:     target,
	}, nil
}

// PluginChannel is one transport session implemented by a resident plugin
// process. It implements channel.Channel; the zero value is not usable.
type PluginChannel struct {
	name       string
	binaryPath string
	configYAML string
	target     channel.Target

	// mu guards sess, the session handle. Request serialisation lives on the
	// session itself; this lock only protects the handle against Connect and
	// Close racing each other.
	mu   sync.Mutex
	sess *pluginSession
}

func (c *PluginChannel) session() *pluginSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// Connect starts the plugin session and hands it the target: process start,
// init (install-time config) and the plugin's own dial. It is idempotent by
// contract — connecting an already-connected channel returns nil.
func (c *PluginChannel) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sess != nil && c.sess.Alive() {
		return nil
	}
	if c.sess != nil {
		// A previous session died (plugin crash, cancelled request). Drop the
		// handle before re-dialling so the stale process is reaped.
		c.sess.close()
		c.sess = nil
	}

	connectCtx, cancel := context.WithTimeout(ctx, sessionConnectDeadline)
	defer cancel()

	sess, err := startPluginSession(connectCtx, c.name, c.binaryPath, c.configYAML)
	if err != nil {
		return err
	}
	resp, err := sess.Request(connectCtx, HostRequest{Action: "connect", Params: channelConnectParams(c.target)})
	if err != nil {
		sess.kill()
		return wrapError(c.name, fmt.Errorf("connect: %w", err))
	}
	if !resp.OK {
		sess.kill()
		return wrapError(c.name, fmt.Errorf("connect refused: %s", resp.Error))
	}
	c.sess = sess
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
		return nil, wrapError(c.name, fmt.Errorf("channel: not connected"))
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
		return &channel.ExecResult{ExitCode: -1}, wrapError(c.name, fmt.Errorf("exec refused: %s", resp.Error))
	}
	if resp.Exec == nil {
		return &channel.ExecResult{ExitCode: -1}, wrapError(c.name, fmt.Errorf("exec returned ok with no result; an empty success is not a verdict"))
	}
	return &channel.ExecResult{
		ExitCode: resp.Exec.ExitCode,
		Stdout:   resp.Exec.Stdout,
		Stderr:   resp.Exec.Stderr,
		Duration: time.Duration(resp.Exec.Duration) * time.Millisecond,
	}, nil
}

// Upload is not part of the declared ChannelPlugin contract, so a plugin
// channel refuses it by name instead of pretending: steps that push files
// (file.copy, file.template) cannot run against a plugin transport today.
func (c *PluginChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	return wrapError(c.name, fmt.Errorf("channel: upload is not supported by plugin channels (the ChannelPlugin contract has no upload verb); use a built-in transport for file.push steps"))
}

// Download retrieves a path from the target through the plugin's collect verb
// and hands the caller a reader over the decoded bytes.
func (c *PluginChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	sess := c.session()
	if sess == nil || !sess.Alive() {
		return nil, wrapError(c.name, fmt.Errorf("channel: not connected"))
	}

	resp, err := sess.Request(ctx, HostRequest{Action: "collect", Params: map[string]any{"path": remotePath}})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, wrapError(c.name, fmt.Errorf("collect refused: %s", resp.Error))
	}
	if resp.Collect == nil {
		return nil, wrapError(c.name, fmt.Errorf("collect returned ok with no payload; an empty success is not a verdict"))
	}
	data, err := base64.StdEncoding.DecodeString(resp.Collect.Data)
	if err != nil {
		return nil, wrapError(c.name, fmt.Errorf("collect payload is not base64: %w", err))
	}
	return bytes.NewReader(data), nil
}

// Close disconnects and reaps the session. It is idempotent by contract and
// never fails on teardown: the run is done with the session either way.
func (c *PluginChannel) Close() error {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()

	if sess == nil {
		return nil
	}
	if sess.Alive() {
		ctx, cancel := context.WithTimeout(context.Background(), sessionTeardownDeadline)
		_, _ = sess.Request(ctx, HostRequest{Action: "disconnect"})
		cancel()
	}
	sess.close()
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
