//go:build pkgreplay

package pkg

// Opt-in real Alpine proof, driven by scripts/replay_pg_schema.py --suite pkg-alpine.

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/channel"
	sshchannel "github.com/nexus/levee/internal/channel/ssh"
	"github.com/nexus/levee/internal/executor"
)

type alpineReplayTarget struct {
	port int
	key  string
}

func (t alpineReplayTarget) Host() string { return "127.0.0.1" }
func (t alpineReplayTarget) Port() int    { return t.port }
func (t alpineReplayTarget) Type() string { return "ssh" }
func (t alpineReplayTarget) Credentials() channel.CredentialRef {
	return channel.CredentialRef{Username: "root", KeyPath: t.key}
}

func TestAlpinePackageManagerReplay(t *testing.T) {
	if os.Getenv("LEVEE_PG_REPLAY") != "1" {
		t.Skip("run scripts/replay_pg_schema.py --suite pkg-alpine for the real apk proof")
	}
	port, err := strconv.Atoi(os.Getenv("LEVEE_PG_REPLAY_PORT"))
	require.NoError(t, err)
	target := alpineReplayTarget{port: port, key: os.Getenv("LEVEE_PG_REPLAY_KEY")}
	transport, err := sshchannel.NewChannel(target, &sshchannel.Config{
		StrictHostCheck: true,
		KnownHostsPath:  os.Getenv("LEVEE_PG_REPLAY_KNOWN_HOSTS"),
		ConnectTimeout:  10 * time.Second,
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, transport.Connect(ctx))
	t.Cleanup(func() { _ = transport.Close() })
	input := executor.ModuleInput{Target: target, Channel: transport}
	withArgs := func(args map[string]any) executor.ModuleInput {
		input.Args = args
		return input
	}

	// tree is small, non-essential, and available in Alpine's main repository.
	// The target is disposable, so this proves real apk state transitions without
	// touching the host or depending on BusyBox removal semantics.
	first, err := New().Execute(ctx, "install", withArgs(map[string]any{"name": "tree"}))
	require.NoError(t, err)
	require.Zero(t, first.ExitCode, first.Stderr)
	assert.True(t, first.Changed)

	second, err := New().Execute(ctx, "install", withArgs(map[string]any{"name": "tree"}))
	require.NoError(t, err)
	require.Zero(t, second.ExitCode, second.Stderr)
	assert.False(t, second.Changed, "an already installed package must be a no-op")

	upgraded, err := New().Execute(ctx, "upgrade", withArgs(map[string]any{"name": "tree"}))
	require.NoError(t, err)
	require.Zero(t, upgraded.ExitCode, upgraded.Stderr)
	assert.True(t, upgraded.Changed)

	removed, err := New().Execute(ctx, "remove", withArgs(map[string]any{"name": "tree"}))
	require.NoError(t, err)
	require.Zero(t, removed.ExitCode, removed.Stderr)
	assert.True(t, removed.Changed)

	removedAgain, err := New().Execute(ctx, "remove", withArgs(map[string]any{"name": "tree"}))
	require.NoError(t, err)
	require.Zero(t, removedAgain.ExitCode, removedAgain.Stderr)
	assert.False(t, removedAgain.Changed, "an already absent package must be a no-op")
	t.Log("PKG_APK_REPLAY PASS: install, reinstall, upgrade, remove, remove-again")
}
