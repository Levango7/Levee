// channel_test.go pins the channel plugin adapter — the one consumption point
// that is a session rather than a one-shot. It covers the factory's boot-time
// checks, the addressing handed to the plugin (and the plaintext boundary),
// the Exec/Download mappings and their fail-closed refusals, Close/IsConnected
// semantics, cancellation killing the session, and request serialisation under
// concurrent Exec. The child binary is the test itself re-executed
// (protocol_test.go's "channel" personality).
package plugin

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// channelRecord builds the registry row the factory consumes. BinaryPath
// points at the test binary; the child personality is selected by childEnvKey.
func channelRecord(name string) *RegistryRecord {
	return &RegistryRecord{
		Name:       name,
		Type:       TypeChannel,
		State:      StateEnabled,
		BinaryPath: os.Args[0],
	}
}

// channelTestTarget is the addressing a channel factory receives.
type channelTestTarget struct {
	host string
	port int
	typ  string
	cred channel.CredentialRef
}

func (t channelTestTarget) Host() string                       { return t.host }
func (t channelTestTarget) Port() int                          { return t.port }
func (t channelTestTarget) Type() string                       { return t.typ }
func (t channelTestTarget) Credentials() channel.CredentialRef { return t.cred }

// namedTarget additionally exposes the credential reference NAME, which is
// what a plugin channel is allowed to receive.
type namedTarget struct {
	channelTestTarget
	refName string
}

func (t namedTarget) CredentialRefName() string { return t.refName }

func connectFixture(t *testing.T, target channel.Target) *PluginChannel {
	t.Helper()
	t.Setenv(childEnvKey, "channel")

	f, err := NewPluginChannelFactory(context.Background(), channelRecord("acme-rest"))
	require.NoError(t, err)

	ch, err := f.Create(target)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ch.Close() })

	require.NoError(t, ch.Connect(context.Background()))
	return ch.(*PluginChannel)
}

// Only an enabled channel-type record can become a transport.
func TestNewPluginChannelFactoryRefusesBadRecords(t *testing.T) {
	t.Setenv(childEnvKey, "channel")

	_, err := NewPluginChannelFactory(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil registry record")

	rec := channelRecord("wrong-type")
	rec.Type = TypeModule
	_, err = NewPluginChannelFactory(context.Background(), rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidPluginType)

	rec = channelRecord("not-enabled")
	rec.State = StateDisabled
	_, err = NewPluginChannelFactory(context.Background(), rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPluginNotEnabled)
}

// A binary that vanished after install is refused at construction, naming the
// path — the boot-time usability question, not a first-dial surprise.
func TestNewPluginChannelFactoryRefusesMissingBinary(t *testing.T) {
	rec := channelRecord("gone")
	rec.BinaryPath = filepath.Join(t.TempDir(), "not-there")

	_, err := NewPluginChannelFactory(context.Background(), rec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binary not found")
}

// The transport type a plugin serves is its registry name — the value an
// inventory row declares in channel_type.
func TestPluginChannelFactoryNameIsTheRegistryName(t *testing.T) {
	t.Setenv(childEnvKey, "channel")

	f, err := NewPluginChannelFactory(context.Background(), channelRecord("acme-rest"))
	require.NoError(t, err)
	assert.Equal(t, "acme-rest", f.Name())

	_, err = f.Create(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil target")
}

// The plugin receives host/port/type and — when the inventory row names one —
// the credential reference NAME. The plaintext CredentialRef is never sent;
// the child refuses the connect if a "credentials" key appears, so this test
// fails loudly if that boundary is ever crossed.
func TestPluginChannelConnectHandsAddressingWithoutPlaintext(t *testing.T) {
	target := namedTarget{
		channelTestTarget: channelTestTarget{
			host: "api.example.com", port: 443, typ: "acme-rest",
			cred: channel.CredentialRef{Username: "svc", Password: "hunter2"},
		},
		refName: "acme-prod-token",
	}
	ch := connectFixture(t, target)

	res, err := ch.Exec(context.Background(), "status")
	require.NoError(t, err)
	assert.Equal(t, "ran status ref=acme-prod-token", res.Stdout)
}

// The contract makes Connect idempotent: connecting an already-connected
// channel returns nil and keeps the session.
func TestPluginChannelConnectIsIdempotent(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	require.NoError(t, ch.Connect(context.Background()))
	require.NoError(t, ch.Connect(context.Background()))

	res, err := ch.Exec(context.Background(), "still-alive")
	require.NoError(t, err)
	assert.Contains(t, res.Stdout, "ran still-alive")
}

// Before Connect the channel reports disconnected and refuses work; false
// from IsConnected is the caller's signal to dial.
func TestPluginChannelRefusesWorkBeforeConnect(t *testing.T) {
	t.Setenv(childEnvKey, "channel")
	f, err := NewPluginChannelFactory(context.Background(), channelRecord("acme-rest"))
	require.NoError(t, err)
	ch, err := f.Create(channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})
	require.NoError(t, err)

	assert.False(t, ch.IsConnected())
	_, err = ch.Exec(context.Background(), "whoami")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
	_, err = ch.Download(context.Background(), "/etc/hosts")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// A completed command — including a non-zero exit — is a result, not an
// error: the module layer owns that distinction (same as the SSH channel).
func TestPluginChannelExecMapsResult(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	res, err := ch.Exec(context.Background(), "deploy")
	require.NoError(t, err)
	assert.Equal(t, 0, res.ExitCode)
	assert.Contains(t, res.Stdout, "ran deploy")
	assert.Equal(t, 3*time.Millisecond, res.Duration)

	res, err = ch.Exec(context.Background(), "fail")
	require.NoError(t, err, "a non-zero exit is a result, not a transport error")
	assert.Equal(t, 2, res.ExitCode)
	assert.Equal(t, "command failed", res.Stderr)
}

// A plugin that refuses the command reports it; the result carries -1, never a
// faked 0.
func TestPluginChannelExecRefusalIsAnError(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	res, err := ch.Exec(context.Background(), "refuse")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exec refused")
	assert.Contains(t, err.Error(), "target rejected the command")
	require.NotNil(t, res)
	assert.Equal(t, -1, res.ExitCode)
}

// ok:true with no exec payload is not a verdict.
func TestPluginChannelExecWithoutPayloadIsRefused(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	_, err := ch.Exec(context.Background(), "nopayload")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty success is not a verdict")
}

// Upload is outside the declared ChannelPlugin contract, so the channel says
// so by name instead of pretending to transfer.
func TestPluginChannelUploadIsRefusedByName(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	err := ch.Upload(context.Background(), "/tmp/x", io.NopCloser(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload is not supported by plugin channels")
}

// Download maps the plugin's collect verb onto a reader over the decoded bytes.
func TestPluginChannelDownloadDecodesCollectPayload(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	r, err := ch.Download(context.Background(), "/etc/hosts")
	require.NoError(t, err)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "collected:/etc/hosts", string(data))
}

// Close is idempotent by contract; after it the channel is disconnected and
// refuses work rather than talking to a dead session.
func TestPluginChannelCloseIsIdempotent(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})
	require.True(t, ch.IsConnected())

	require.NoError(t, ch.Close())
	require.NoError(t, ch.Close())
	assert.False(t, ch.IsConnected())

	_, err := ch.Exec(context.Background(), "whoami")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// A request that outlives its context kills the session and surfaces the
// context error verbatim: the stream cannot be resynchronised, so the channel
// reports disconnected and the next use re-dials.
func TestPluginChannelCancelledExecKillsSession(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := ch.Exec(ctx, "hang")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	assert.False(t, ch.IsConnected(), "an abandoned request must leave the session dead")
	_, err = ch.Exec(context.Background(), "whoami")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// A plugin process that dies mid-session is a disconnected channel, not a
// channel that keeps answering from a stale pipe.
func TestPluginChannelPluginDeathMarksDisconnected(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	_, err := ch.Exec(context.Background(), "die")
	require.Error(t, err)
	assert.False(t, ch.IsConnected())
}

// The channel contract explicitly allows concurrent Exec; the session
// serialises the requests instead of interleaving writes on one pipe.
func TestPluginChannelConcurrentExecIsSerialised(t *testing.T) {
	ch := connectFixture(t, channelTestTarget{host: "h1", port: 22, typ: "acme-rest"})

	const workers = 4
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := ch.Exec(context.Background(), "cmd-"+string(rune('a'+i)))
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = res.Stdout
		}(i)
	}
	wg.Wait()

	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		assert.Contains(t, results[i], "ran cmd-"+string(rune('a'+i)),
			"each caller must get its own answer, not a neighbour's")
	}
}
