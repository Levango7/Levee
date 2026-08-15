package channel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

// --- ChannelRegistry benchmarks ---

func BenchmarkChannelRegistry_New(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewChannelRegistry()
	}
}

func BenchmarkChannelRegistry_Register(b *testing.B) {
	b.ReportAllocs()
	reg := NewChannelRegistry()
	f := &noopFactory{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reg.Register(fmt.Sprintf("type-%d", i%100), f)
	}
}

func BenchmarkChannelRegistry_Factory(b *testing.B) {
	b.ReportAllocs()
	reg := NewChannelRegistry()
	f := &noopFactory{}
	for i := 0; i < 100; i++ {
		reg.Register(fmt.Sprintf("type-%d", i), f)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = reg.Factory(fmt.Sprintf("type-%d", i%100))
	}
}

func BenchmarkChannelRegistry_Create(b *testing.B) {
	b.ReportAllocs()
	reg := NewChannelRegistry()
	f := &noopFactory{}
	reg.Register("noop", f)
	tgt := &simpleTarget{host: "host1", port: 22, typ: "noop"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = reg.Create(tgt)
	}
}

func BenchmarkChannelRegistry_Types(b *testing.B) {
	b.ReportAllocs()
	reg := NewChannelRegistry()
	f := &noopFactory{}
	for i := 0; i < 20; i++ {
		reg.Register(fmt.Sprintf("type-%02d", i), f)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reg.Types()
	}
}

// --- Limiter benchmarks ---

func BenchmarkLimiter_AcquireRelease(b *testing.B) {
	b.ReportAllocs()
	limiter := NewLimiter(1000, 100, 10, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = limiter.Acquire("ssh", "host1")
		limiter.Release("ssh", "host1")
	}
}

func BenchmarkLimiter_AcquireRelease_Parallel(b *testing.B) {
	b.ReportAllocs()
	limiter := NewLimiter(1000, 100, 10, 0)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			target := fmt.Sprintf("host-%d", i%50)
			_ = limiter.Acquire("ssh", target)
			limiter.Release("ssh", target)
			i++
		}
	})
}

func BenchmarkLimiter_Stats(b *testing.B) {
	b.ReportAllocs()
	limiter := NewLimiter(1000, 100, 10, 0)
	for i := 0; i < 50; i++ {
		_ = limiter.Acquire("ssh", fmt.Sprintf("host-%d", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = limiter.Stats()
	}
}

// --- Prechecker benchmarks ---

func BenchmarkPrechecker_New(b *testing.B) {
	b.ReportAllocs()
	ch := &noopChannel{}
	limiter := NewLimiter(1000, 100, 10, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewPrechecker(ch, limiter)
	}
}

// --- test helpers ---

type noopFactory struct{}

func (f *noopFactory) Create(target Target) (Channel, error) {
	return &noopChannel{}, nil
}

type noopChannel struct{}

func (c *noopChannel) Connect(ctx context.Context) error { return nil }
func (c *noopChannel) Exec(ctx context.Context, cmd string) (*ExecResult, error) {
	return &ExecResult{ExitCode: 0, Stdout: "ok\n"}, nil
}
func (c *noopChannel) Upload(ctx context.Context, remotePath string, content io.Reader) error {
	return nil
}
func (c *noopChannel) Download(ctx context.Context, remotePath string) (io.Reader, error) {
	return strings.NewReader(""), nil
}
func (c *noopChannel) Close() error      { return nil }
func (c *noopChannel) IsConnected() bool { return true }

type simpleTarget struct {
	host string
	port int
	typ  string
}

func (t *simpleTarget) Host() string { return t.host }
func (t *simpleTarget) Port() int    { return t.port }
func (t *simpleTarget) Type() string { return t.typ }
func (t *simpleTarget) Credentials() CredentialRef {
	return CredentialRef{Username: "user", Password: "pass"}
}
