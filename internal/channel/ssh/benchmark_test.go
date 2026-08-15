package ssh

import (
	"testing"

	"github.com/nexus/levee/internal/channel"
)

// --- SSHFactory benchmarks ---

func BenchmarkSSHFactory_Create(b *testing.B) {
	b.ReportAllocs()
	f := &SSHFactory{}
	tgt := &simpleSSHTarget{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = f.Create(tgt)
	}
}

func BenchmarkNewChannel(b *testing.B) {
	b.ReportAllocs()
	tgt := &simpleSSHTarget{}
	cfg := NewConfig()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = NewChannel(tgt, cfg)
	}
}

func BenchmarkNewConfig(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = NewConfig()
	}
}

// --- SSHPool benchmarks ---

func BenchmarkSSHPool_New(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := NewPool(PoolConfig{MaxPerTarget: 5, IdleTimeout: 0, HealthCheckInterval: -1})
		_ = p.Close()
	}
}

func BenchmarkSSHPool_Stats(b *testing.B) {
	b.ReportAllocs()
	p := NewPool(PoolConfig{MaxPerTarget: 5, IdleTimeout: 0, HealthCheckInterval: -1})
	defer p.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = p.Stats()
	}
}

func BenchmarkSSHPool_PoolKey(b *testing.B) {
	b.ReportAllocs()
	tgt := &simpleSSHTarget{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = poolKey(tgt)
	}
}

// --- test helpers ---

type simpleSSHTarget struct{}

func (t *simpleSSHTarget) Host() string         { return "localhost" }
func (t *simpleSSHTarget) Port() int            { return 22 }
func (t *simpleSSHTarget) Type() string         { return "ssh" }
func (t *simpleSSHTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "user", Password: "pass"}
}