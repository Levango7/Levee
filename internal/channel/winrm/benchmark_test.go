package winrm

import (
	"testing"

	"github.com/nexus/levee/internal/channel"
)

// --- WinRMFactory benchmarks ---

func BenchmarkWinRMFactory_Create(b *testing.B) {
	b.ReportAllocs()
	f := NewFactory(Config{})
	tgt := &simpleWinRMTarget{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = f.Create(tgt)
	}
}

func BenchmarkNewChannel(b *testing.B) {
	b.ReportAllocs()
	tgt := &simpleWinRMTarget{}
	cfg := Config{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = newChannel(tgt, cfg)
	}
}

func BenchmarkConfig_WithDefaults(b *testing.B) {
	b.ReportAllocs()
	cfg := Config{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cfg.withDefaults()
	}
}

func BenchmarkConfig_ResolvePort(b *testing.B) {
	b.ReportAllocs()
	cfg := Config{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cfg.resolvePort(0)
	}
}

func BenchmarkFormatISO8601(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = formatISO8601(120)
	}
}

// --- WinRMPool benchmarks ---

func BenchmarkWinRMPool_New(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewPool(nil, PoolConfig{MaxConcurrentPerTarget: 3})
		_ = p.Close()
	}
}

func BenchmarkWinRMPool_Stats(b *testing.B) {
	b.ReportAllocs()
	p := NewPool(nil, PoolConfig{MaxConcurrentPerTarget: 3})
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Stats()
	}
}

func BenchmarkWinRMPool_PoolKey(b *testing.B) {
	b.ReportAllocs()
	tgt := &simpleWinRMTarget{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = poolKey(tgt)
	}
}

// --- test helpers ---

type simpleWinRMTarget struct{}

func (t *simpleWinRMTarget) Host() string { return "winhost" }
func (t *simpleWinRMTarget) Port() int    { return 5985 }
func (t *simpleWinRMTarget) Type() string { return "winrm" }
func (t *simpleWinRMTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "admin", Password: "pass"}
}
