package winrm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
)

// --- test doubles -----------------------------------------------------------

// fakeTarget is a Target stub for WinRM tests.
type fakeTarget struct {
	host string
	port int
	typ  string
	cred channel.CredentialRef
}

func (t fakeTarget) Host() string                       { return t.host }
func (t fakeTarget) Port() int                          { return t.port }
func (t fakeTarget) Type() string                       { return t.typ }
func (t fakeTarget) Credentials() channel.CredentialRef { return t.cred }

// newFakeTarget returns a fakeTarget with sensible defaults for WinRM tests.
func newFakeTarget(host string) fakeTarget {
	return fakeTarget{
		host: host,
		port: 0, // let Config resolve
		typ:  "winrm",
		cred: channel.CredentialRef{Username: "Administrator", Password: "P@ssw0rd!"},
	}
}

// newFakeConnectedChannel returns a WinRMChannel that reports itself as
// connected but has no real winrm.Client. It is suitable for testing pool
// bookkeeping and lifecycle logic that does not actually call Exec / Upload /
// Download. The channel's Close flips connected to false, so IsConnected
// behaves realistically.
func newFakeConnectedChannel(t channel.Target) *WinRMChannel {
	return &WinRMChannel{
		target:    t,
		cfg:       Config{}.withDefaults(),
		connected: true,
	}
}

// --- Config tests -----------------------------------------------------------

func TestConfigWithDefaults(t *testing.T) {
	c := Config{}.withDefaults()
	if c.Transport != DefaultTransport {
		t.Errorf("Transport = %q, want %q", c.Transport, DefaultTransport)
	}
	if c.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}

	// Explicit values are preserved.
	c2 := Config{Transport: TransportNTLM, Timeout: 10 * time.Second}.withDefaults()
	if c2.Transport != TransportNTLM {
		t.Errorf("Transport = %q, want %q", c2.Transport, TransportNTLM)
	}
	if c2.Timeout != 10*time.Second {
		t.Errorf("Timeout = %v, want 10s", c2.Timeout)
	}

	// Transport is lower-cased.
	c3 := Config{Transport: "NTLM"}.withDefaults()
	if c3.Transport != "ntlm" {
		t.Errorf("Transport = %q, want %q", c3.Transport, "ntlm")
	}
}

func TestConfigResolvePort(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		targetPort int
		want       int
	}{
		{"target wins", Config{Port: 5999, HTTPS: true}, 5985, 5985},
		{"cfg port", Config{Port: 5999}, 0, 5999},
		{"https default", Config{HTTPS: true}, 0, DefaultHTTPSPort},
		{"http default", Config{}, 0, DefaultHTTPPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.resolvePort(tt.targetPort)
			if got != tt.want {
				t.Errorf("resolvePort(%d) = %d, want %d", tt.targetPort, got, tt.want)
			}
		})
	}
}

func TestFormatISO8601(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "PT60S"},
		{-time.Second, "PT60S"},
		{30 * time.Second, "PT30S"},
		{120 * time.Second, "PT120S"},
		{1500 * time.Millisecond, "PT1S"}, // truncates sub-second
	}
	for _, tt := range tests {
		got := formatISO8601(tt.d)
		if got != tt.want {
			t.Errorf("formatISO8601(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// --- WinRMFactory tests -----------------------------------------------------

func TestWinRMFactoryCreateValidation(t *testing.T) {
	f := NewFactory(Config{})

	tests := []struct {
		name   string
		target channel.Target
		want   string // substring of error
	}{
		{"nil target", nil, "target is nil"},
		{"empty host", fakeTarget{host: "", typ: "winrm", cred: channel.CredentialRef{Username: "u"}}, "host is empty"},
		{"empty username", fakeTarget{host: "h", typ: "winrm"}, "username is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.Create(tt.target)
			if err == nil {
				t.Fatalf("Create(%v) returned nil error, want error containing %q", tt.name, tt.want)
			}
			if !contains(err.Error(), tt.want) {
				t.Errorf("Create(%v) error = %q, want substring %q", tt.name, err.Error(), tt.want)
			}
		})
	}
}

func TestWinRMFactoryCreateBadTransport(t *testing.T) {
	f := NewFactory(Config{Transport: "kerberos"})
	tgt := newFakeTarget("h1")
	_, err := f.Create(tgt)
	if err == nil {
		t.Fatal("Create with bad transport returned nil error")
	}
	if !contains(err.Error(), "unsupported transport") {
		t.Errorf("error = %q, want substring 'unsupported transport'", err.Error())
	}
}

func TestWinRMFactoryCreateSuccess(t *testing.T) {
	f := NewFactory(Config{Transport: TransportNTLM, HTTPS: true})
	tgt := newFakeTarget("h1")
	ch, err := f.Create(tgt)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	wc, ok := ch.(*WinRMChannel)
	if !ok {
		t.Fatalf("Create returned %T, want *WinRMChannel", ch)
	}
	if wc.IsConnected() {
		t.Error("new channel should not be connected")
	}
	if wc.Target().Host() != "h1" {
		t.Errorf("Target().Host() = %q, want %q", wc.Target().Host(), "h1")
	}
}

func TestWinRMFactoryImplementsChannelFactory(t *testing.T) {
	var _ channel.ChannelFactory = (*WinRMFactory)(nil)
}

// --- WinRMChannel lifecycle tests (no real connection) ---------------------

func TestWinRMChannelExecNotConnected(t *testing.T) {
	ch := &WinRMChannel{target: newFakeTarget("h1"), cfg: Config{}.withDefaults()}
	_, err := ch.Exec(context.Background(), "whoami")
	if err == nil {
		t.Fatal("Exec on unconnected channel returned nil error")
	}
	if !contains(err.Error(), "not connected") {
		t.Errorf("error = %q, want 'not connected'", err.Error())
	}
}

func TestWinRMChannelUploadNotConnected(t *testing.T) {
	ch := &WinRMChannel{target: newFakeTarget("h1"), cfg: Config{}.withDefaults()}
	err := ch.Upload(context.Background(), "C:/test.txt", bytes.NewReader([]byte("hi")))
	if err == nil {
		t.Fatal("Upload on unconnected channel returned nil error")
	}
	if !contains(err.Error(), "not connected") {
		t.Errorf("error = %q, want 'not connected'", err.Error())
	}
}

func TestWinRMChannelDownloadNotConnected(t *testing.T) {
	ch := &WinRMChannel{target: newFakeTarget("h1"), cfg: Config{}.withDefaults()}
	_, err := ch.Download(context.Background(), "C:/test.txt")
	if err == nil {
		t.Fatal("Download on unconnected channel returned nil error")
	}
	if !contains(err.Error(), "not connected") {
		t.Errorf("error = %q, want 'not connected'", err.Error())
	}
}

func TestWinRMChannelCloseIdempotent(t *testing.T) {
	ch := newFakeConnectedChannel(newFakeTarget("h1"))
	if err := ch.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if ch.IsConnected() {
		t.Error("IsConnected() = true after Close")
	}
}

func TestWinRMChannelIsConnected(t *testing.T) {
	ch := &WinRMChannel{target: newFakeTarget("h1"), cfg: Config{}.withDefaults()}
	if ch.IsConnected() {
		t.Error("IsConnected() = true on fresh channel")
	}
	ch.connected = true
	if !ch.IsConnected() {
		t.Error("IsConnected() = false after setting connected")
	}
}

func TestWinRMChannelExecRespectsCancelledContext(t *testing.T) {
	ch := newFakeConnectedChannel(newFakeTarget("h1"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err := ch.Exec(ctx, "whoami")
	// ctx.Err() is checked first; we expect context.Canceled. The fake channel
	// has no real client so we cannot actually run, but the ctx.Err() guard
	// fires before the client check.
	if err == nil {
		t.Fatal("Exec with cancelled ctx returned nil error")
	}
}

// --- WinRMPool tests --------------------------------------------------------

// newTestPool returns a WinRMPool whose createFunc returns fake connected
// channels (no real winrm.Client). The createCount pointer is incremented on
// each creation so tests can assert how many channels were built.
func newTestPool(maxPerTarget int, createCount *int32) *WinRMPool {
	p := NewPool(nil, PoolConfig{MaxConcurrentPerTarget: maxPerTarget})
	p.createFunc = func(ctx context.Context, target channel.Target) (*WinRMChannel, error) {
		atomic.AddInt32(createCount, 1)
		return newFakeConnectedChannel(target), nil
	}
	return p
}

func TestPoolGetCreatesChannel(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	ch, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ch.IsConnected() {
		t.Error("returned channel is not connected")
	}
	if atomic.LoadInt32(&created) != 1 {
		t.Errorf("created = %d, want 1", created)
	}
	p.Put(ch)
}

func TestPoolGetReusesIdleChannel(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	ch1, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	p.Put(ch1)

	// Second Get should reuse the idle channel, not create a new one.
	ch2, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if atomic.LoadInt32(&created) != 1 {
		t.Errorf("created = %d, want 1 (should reuse)", created)
	}
	p.Put(ch2)
}

func TestPoolGetMultipleTargetsSeparateBuckets(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()

	t1 := newFakeTarget("h1")
	t2 := newFakeTarget("h2")

	ch1, err := p.Get(context.Background(), t1)
	if err != nil {
		t.Fatalf("Get h1: %v", err)
	}
	ch2, err := p.Get(context.Background(), t2)
	if err != nil {
		t.Fatalf("Get h2: %v", err)
	}
	if ch1 == ch2 {
		t.Error("channels for different targets should be distinct")
	}
	if atomic.LoadInt32(&created) != 2 {
		t.Errorf("created = %d, want 2", created)
	}
	p.Put(ch1)
	p.Put(ch2)
}

func TestPoolConcurrencyLimit(t *testing.T) {
	var created int32
	maxPerTarget := 2
	p := newTestPool(maxPerTarget, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	ctx := context.Background()

	// Acquire up to the limit.
	ch1, err := p.Get(ctx, tgt)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	ch2, err := p.Get(ctx, tgt)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}

	// Third Get should block because the limit is reached. Use a short timeout
	// to verify it does not return immediately.
	quickCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	_, err = p.Get(quickCtx, tgt)
	cancel()
	if err == nil {
		t.Fatal("Get 3 should have timed out or been cancelled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Get 3 error = %v, want context.DeadlineExceeded", err)
	}

	// After putting one back, a new Get should succeed (reuse).
	p.Put(ch1)
	ch3, err := p.Get(ctx, tgt)
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	p.Put(ch2)
	p.Put(ch3)

	if atomic.LoadInt32(&created) != 2 {
		t.Errorf("created = %d, want 2 (within limit)", created)
	}
}

func TestPoolGetCancelledContext(t *testing.T) {
	var created int32
	p := newTestPool(1, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	// Occupy the single slot.
	ch, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}

	// Cancel before calling Get; should return context.Canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Get(ctx, tgt)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Get with cancelled ctx = %v, want context.Canceled", err)
	}
	p.Put(ch)
}

func TestPoolCloseReturnsError(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := p.Get(context.Background(), newFakeTarget("h1"))
	if !errors.Is(err, errPoolClosed) {
		t.Errorf("Get after Close = %v, want errPoolClosed", err)
	}
}

func TestPoolCloseIdempotent(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	if err := p.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

func TestPoolPutAfterCloseClosesChannel(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	tgt := newFakeTarget("h1")
	ch, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = p.Close()
	p.Put(ch) // should close ch, not panic
	if ch.IsConnected() {
		t.Error("channel should be closed after Put on closed pool")
	}
}

func TestPoolPutNilIsNoop(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()
	p.Put(nil) // must not panic
}

func TestPoolHealthCheckDiscardsStaleChannel(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	ch, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	// Simulate the channel going stale while idle.
	_ = ch.Close()
	p.Put(ch) // put a closed channel back into idle

	// Next Get should discard the stale channel and create a fresh one.
	ch2, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if !ch2.IsConnected() {
		t.Error("Get 2 should return a healthy channel")
	}
	if atomic.LoadInt32(&created) != 2 {
		t.Errorf("created = %d, want 2 (stale discarded, new created)", created)
	}
	p.Put(ch2)
}

func TestPoolHealthCheckDisabledReusesStaleChannel(t *testing.T) {
	var created int32
	p := NewPool(nil, PoolConfig{MaxConcurrentPerTarget: 3, DisableHealthCheck: true})
	defer p.Close()
	p.createFunc = func(ctx context.Context, target channel.Target) (*WinRMChannel, error) {
		atomic.AddInt32(&created, 1)
		return newFakeConnectedChannel(target), nil
	}

	tgt := newFakeTarget("h1")
	ch, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	_ = ch.Close()
	p.Put(ch)

	// With health check disabled, the stale channel is handed back as-is.
	ch2, err := p.Get(context.Background(), tgt)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if ch2.IsConnected() {
		t.Error("stale channel should still be stale (health check disabled)")
	}
	if atomic.LoadInt32(&created) != 1 {
		t.Errorf("created = %d, want 1 (no health check, no new creation)", created)
	}
	p.Put(ch2)
}

func TestPoolStats(t *testing.T) {
	var created int32
	p := newTestPool(2, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	ch1, _ := p.Get(context.Background(), tgt)
	ch2, _ := p.Get(context.Background(), tgt)

	stats := p.Stats()
	if len(stats) != 1 {
		t.Fatalf("len(Stats) = %d, want 1", len(stats))
	}
	s := stats[0]
	if s.Max != 2 {
		t.Errorf("Max = %d, want 2", s.Max)
	}
	if s.InUse != 2 {
		t.Errorf("InUse = %d, want 2", s.InUse)
	}
	if s.Idle != 0 {
		t.Errorf("Idle = %d, want 0", s.Idle)
	}

	p.Put(ch1)
	stats = p.Stats()
	if stats[0].InUse != 1 || stats[0].Idle != 1 {
		t.Errorf("after Put: InUse=%d Idle=%d, want 1/1", stats[0].InUse, stats[0].Idle)
	}
	p.Put(ch2)
}

func TestPoolConcurrentGetPut(t *testing.T) {
	var created int32
	p := newTestPool(3, &created)
	defer p.Close()

	tgt := newFakeTarget("h1")
	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				ch, err := p.Get(context.Background(), tgt)
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				// Simulate single-connection-single-command: use then put.
				p.Put(ch)
			}
		}()
	}
	wg.Wait()

	// The pool should never have created more than maxPerTarget channels.
	if c := atomic.LoadInt32(&created); c > 3 {
		t.Errorf("created = %d, should never exceed max=3", c)
	}
}

// --- registry integration ---------------------------------------------------

func TestWinRMRegisteredInDefaultRegistry(t *testing.T) {
	f, ok := channel.DefaultRegistry().Factory("winrm")
	if !ok {
		t.Fatal("no winrm factory registered in default registry")
	}
	if _, ok := f.(*WinRMFactory); !ok {
		t.Errorf("registered factory is %T, want *WinRMFactory", f)
	}
}

// --- helpers ----------------------------------------------------------------

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Ensure unused imports are referenced (io used in fakeConnectedChannel via
// bytes.Reader in other test files potentially).
var _ = io.EOF
var _ = fmt.Sprintf
