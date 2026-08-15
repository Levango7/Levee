package channel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- test doubles ----------------------------------------------------------

// staticTarget is a Target stub used in registry tests.
type staticTarget struct {
	host string
	port int
	typ  string
	cred CredentialRef
}

func (t staticTarget) Host() string               { return t.host }
func (t staticTarget) Port() int                  { return t.port }
func (t staticTarget) Type() string               { return t.typ }
func (t staticTarget) Credentials() CredentialRef { return t.cred }

// fakeChannel is a Channel stub that records its lifecycle.
type fakeChannel struct {
	connected bool
	closed    bool
}

func (c *fakeChannel) Connect(ctx context.Context) error { c.connected = true; return nil }
func (c *fakeChannel) Exec(ctx context.Context, cmd string) (*ExecResult, error) {
	return &ExecResult{ExitCode: 0, Stdout: cmd, Duration: 1 * time.Millisecond}, nil
}
func (c *fakeChannel) Upload(ctx context.Context, p string, r io.Reader) error { return nil }
func (c *fakeChannel) Download(ctx context.Context, p string) (io.Reader, error) {
	return io.NopCloser(new(bytes.Reader)), nil
}
func (c *fakeChannel) Close() error      { c.closed = true; c.connected = false; return nil }
func (c *fakeChannel) IsConnected() bool { return c.connected }

// fakeFactory hands out fakeChannel instances and counts how many it created.
type fakeFactory struct{ created int }

func (f *fakeFactory) Create(t Target) (Channel, error) {
	f.created++
	return &fakeChannel{}, nil
}

// failingFactory always returns an error to exercise the error path.
type failingFactory struct{ err error }

func (f *failingFactory) Create(t Target) (Channel, error) { return nil, f.err }

// --- registry tests --------------------------------------------------------

func TestNewChannelRegistryEmpty(t *testing.T) {
	r := NewChannelRegistry()
	assert.Empty(t, r.Types())
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewChannelRegistry()
	f := &fakeFactory{}
	r.Register("ssh", f)

	got, ok := r.Factory("ssh")
	require.True(t, ok)
	assert.Same(t, f, got)

	_, ok = r.Factory("winrm")
	assert.False(t, ok)
}

func TestRegistryOverwrite(t *testing.T) {
	r := NewChannelRegistry()
	a := &fakeFactory{}
	b := &fakeFactory{}
	r.Register("ssh", a)
	r.Register("ssh", b) // overwrite
	got, ok := r.Factory("ssh")
	require.True(t, ok)
	assert.Same(t, b, got)
}

func TestRegistryUnregister(t *testing.T) {
	r := NewChannelRegistry()
	r.Register("ssh", &fakeFactory{})
	r.Unregister("ssh")
	_, ok := r.Factory("ssh")
	assert.False(t, ok)
}

func TestRegistryUnregisterMissingIsNoop(t *testing.T) {
	r := NewChannelRegistry()
	r.Unregister("never-registered") // must not panic
}

func TestRegistryCreateDelegates(t *testing.T) {
	r := NewChannelRegistry()
	f := &fakeFactory{}
	r.Register("ssh", f)

	tgt := staticTarget{host: "h1", port: 22, typ: "ssh"}
	ch, err := r.Create(tgt)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 1, f.created)
}

func TestRegistryCreateUnknownType(t *testing.T) {
	r := NewChannelRegistry()
	tgt := staticTarget{host: "h1", port: 22, typ: "ipmi"}
	_, err := r.Create(tgt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ipmi")
}

func TestRegistryCreateFactoryError(t *testing.T) {
	r := NewChannelRegistry()
	want := errors.New("boom")
	r.Register("broken", &failingFactory{err: want})
	_, err := r.Create(staticTarget{typ: "broken"})
	require.ErrorIs(t, err, want)
}

func TestRegistryTypesSorted(t *testing.T) {
	r := NewChannelRegistry()
	r.Register("winrm", &fakeFactory{})
	r.Register("ssh", &fakeFactory{})
	r.Register("agent", &fakeFactory{})
	assert.Equal(t, []string{"agent", "ssh", "winrm"}, r.Types())
}

func TestRegistryConcurrentRegister(t *testing.T) {
	r := NewChannelRegistry()
	var wg sync.WaitGroup
	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r.Register("ssh", &fakeFactory{})
			_, _ = r.Factory("ssh")
		}()
	}
	wg.Wait()
	// Final state must be consistent: exactly one factory registered.
	_, ok := r.Factory("ssh")
	assert.True(t, ok)
}

// --- default registry ------------------------------------------------------

func TestDefaultRegistryIsSingleton(t *testing.T) {
	a := DefaultRegistry()
	b := DefaultRegistry()
	assert.Same(t, a, b)
}

func TestDefaultRegistryRegisterCreate(t *testing.T) {
	// Use a unique type name to avoid colliding with any real transport that
	// may register itself via init().
	r := DefaultRegistry()
	f := &fakeFactory{}
	r.Register("test-transport", f)
	defer r.Unregister("test-transport")

	ch, err := r.Create(staticTarget{typ: "test-transport"})
	require.NoError(t, err)
	require.NotNil(t, ch)
}

// --- Target / CredentialRef sanity -----------------------------------------

func TestCredentialRefZeroValue(t *testing.T) {
	var c CredentialRef
	assert.Empty(t, c.Username)
	assert.Empty(t, c.Password)
	assert.Empty(t, c.KeyPath)
	assert.Empty(t, c.KeyPassphrase)
}

func TestStaticTargetAccessors(t *testing.T) {
	cred := CredentialRef{Username: "root", KeyPath: "/tmp/id_rsa"}
	tgt := staticTarget{host: "10.0.0.1", port: 22, typ: "ssh", cred: cred}
	assert.Equal(t, "10.0.0.1", tgt.Host())
	assert.Equal(t, 22, tgt.Port())
	assert.Equal(t, "ssh", tgt.Type())
	assert.Equal(t, cred, tgt.Credentials())
}

// --- ExecResult ------------------------------------------------------------

func TestExecResultFields(t *testing.T) {
	r := ExecResult{ExitCode: 2, Stdout: "out", Stderr: "err", Duration: 3 * time.Millisecond}
	assert.Equal(t, 2, r.ExitCode)
	assert.Equal(t, "out", r.Stdout)
	assert.Equal(t, "err", r.Stderr)
	assert.Equal(t, 3*time.Millisecond, r.Duration)
}
