package winrm

// D-9 wiring tests: the registered WinRM factory must read the process-wide
// defaults installed by SetDefaultConfig at Create time (not the old init-time
// Config{} snapshot), so cfg.Channel.WinRM reaches real channels.

import (
	"testing"
	"time"

	"github.com/nexus/levee/internal/channel"
)

func TestSetDefaultConfigReachesRegisteredFactory(t *testing.T) {
	// Save & restore process defaults (test isolation).
	oldPort, oldTransport, oldTimeout, oldPool := defaultPort, defaultTransport, defaultTimeout, defaultPoolSize
	t.Cleanup(func() {
		SetDefaultConfig(oldPort, oldTransport, oldTimeout, oldPool)
	})

	SetDefaultConfig(7599, "ntlm", 7*time.Second, 9)

	// Create via the registered factory (the path serve uses).
	f, ok := channel.DefaultRegistry().Factory("winrm")
	if !ok {
		t.Fatal("winrm factory not registered")
	}
	ch, err := f.Create(newFakeTarget("h1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wch := ch.(*WinRMChannel)
	got := wch.cfg.withDefaults()
	if got.Port != 7599 {
		t.Errorf("port = %d, want 7599", got.Port)
	}
	if got.Transport != "ntlm" {
		t.Errorf("transport = %q, want ntlm", got.Transport)
	}
	if got.Timeout != 7*time.Second {
		t.Errorf("timeout = %v, want 7s", got.Timeout)
	}

	// target port still wins over the configured default.
	withTarget := newFakeTarget("h2")
	withTarget.port = 8985
	ch2, _ := f.Create(withTarget)
	if wch2 := ch2.(*WinRMChannel); wch2.cfg.resolvePort(withTarget.port) != 8985 {
		t.Errorf("target port must win")
	}
}

func TestSetDefaultConfigUnsetKeepsHistoricalDefaults(t *testing.T) {
	oldPort, oldTransport, oldTimeout, oldPool := defaultPort, defaultTransport, defaultTimeout, defaultPoolSize
	t.Cleanup(func() { SetDefaultConfig(oldPort, oldTransport, oldTimeout, oldPool) })

	SetDefaultConfig(0, "", 0, 0)
	f, _ := channel.DefaultRegistry().Factory("winrm")
	ch, _ := f.Create(newFakeTarget("h1"))
	got := ch.(*WinRMChannel).cfg.withDefaults()
	if got.Transport != DefaultTransport {
		t.Errorf("unset transport = %q, want %q", got.Transport, DefaultTransport)
	}
	if got.Timeout != DefaultTimeout {
		t.Errorf("unset timeout = %v, want %v", got.Timeout, DefaultTimeout)
	}
	if got.resolvePort(0) != DefaultHTTPPort {
		t.Errorf("unset port = %d, want %d", got.resolvePort(0), DefaultHTTPPort)
	}
}

func TestDefaultPoolSizeUsedByPool(t *testing.T) {
	oldPort, oldTransport, oldTimeout, oldPool := defaultPort, defaultTransport, defaultTimeout, defaultPoolSize
	t.Cleanup(func() { SetDefaultConfig(oldPort, oldTransport, oldTimeout, oldPool) })

	SetDefaultConfig(0, "", 0, 9)
	p := NewPool(nil, PoolConfig{}) // nil factory -> newDefaultFactory
	if got := p.cfg.MaxConcurrentPerTarget; got != 9 {
		t.Errorf("pool max = %d, want configured 9", got)
	}

	SetDefaultConfig(0, "", 0, 0) // unset -> historical 3
	p2 := NewPool(nil, PoolConfig{})
	if got := p2.cfg.MaxConcurrentPerTarget; got != 3 {
		t.Errorf("pool max = %d, want historical 3", got)
	}
}
