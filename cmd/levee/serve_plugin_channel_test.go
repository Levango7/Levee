// serve_plugin_channel_test.go pins the serve half of the channel plugin
// consumption point: missing registry = unconfigured (no-op), an enabled
// channel plugin is registered into the process-wide channel registry under
// its own name and builds channels, other types are filtered out before any
// binary is touched, a name that would shadow a built-in transport is refused,
// and an enabled channel whose binary vanished refuses the boot with the
// escape hatch printed.
//
// The child binary is the test binary re-executed through the personality
// TestMain already routes (serve_plugin_module_test.go); the channel factory
// only needs the meta handshake, which that personality answers.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/plugin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// No registry file: no plugin was ever installed, so there is nothing to do.
func TestInstallPluginChannelsMissingRegistryIsNoop(t *testing.T) {
	cfg := pluginNotifyFixture(t, nil)

	require.NoError(t, installPluginChannels(context.Background(), cfg))
}

// An enabled channel plugin becomes a transport: its name is the channel_type
// an inventory row declares, and the factory it registers builds channels.
func TestInstallPluginChannelsRegistersEnabledChannel(t *testing.T) {
	t.Setenv(serveModuleChildEnv, "1")
	const name = "serve-test-acme-rest"
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name: name, Type: plugin.TypeChannel, BinaryPath: os.Args[0],
		})
	})

	require.NoError(t, installPluginChannels(context.Background(), cfg))
	t.Cleanup(func() { channel.DefaultRegistry().Unregister(name) })

	f, ok := channel.DefaultRegistry().Factory(name)
	require.True(t, ok, "the enabled channel plugin must be a dialable transport")
	assert.Equal(t, name, f.(*plugin.PluginChannelFactory).Name())

	ch, err := f.Create(channelTestTarget{})
	require.NoError(t, err)
	assert.False(t, ch.IsConnected(), "Create returns an unconnected channel by contract")
}

// channelTestTarget is the minimal Target the registry needs to build a
// channel; nothing here is dialled.
type channelTestTarget struct{}

func (channelTestTarget) Host() string                       { return "inventory-row" }
func (channelTestTarget) Port() int                          { return 0 }
func (channelTestTarget) Type() string                       { return "serve-test-acme-rest" }
func (channelTestTarget) Credentials() channel.CredentialRef { return channel.CredentialRef{} }

// Types other than "channel" are filtered out before their binary is looked
// at: the binary here does not exist, so a probe would have failed.
func TestInstallPluginChannelsSkipsOtherTypes(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name:       "serve-test-module",
			Type:       plugin.TypeModule,
			BinaryPath: filepath.Join(t.TempDir(), "absent"),
		})
	})

	require.NoError(t, installPluginChannels(context.Background(), cfg))
	_, ok := channel.DefaultRegistry().Factory("serve-test-module")
	assert.False(t, ok, "a module plugin must never become a transport")
}

// A plugin whose name shadows a registered transport is refused before
// registration: Register overwrites by design, and silently replacing "ssh"
// would route every workflow written against it through a plugin.
func TestInstallPluginChannelsRefusesBuiltinCollision(t *testing.T) {
	_, ok := channel.DefaultRegistry().Factory("ssh")
	require.True(t, ok, "precondition: the built-in ssh transport is registered in this binary")

	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name: "ssh", Type: plugin.TypeChannel, BinaryPath: os.Args[0],
		})
	})

	err := installPluginChannels(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with an already-registered channel type")
	assert.Contains(t, err.Error(), "levee plugin disable ssh")
}

// An enabled channel whose binary vanished is unusable: the boot is refused
// with the plugin named and the escape hatch, not discovered on the first dial
// hours later.
func TestInstallPluginChannelsRefusesMissingBinary(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name:       "serve-test-vanished",
			Type:       plugin.TypeChannel,
			BinaryPath: filepath.Join(t.TempDir(), "gone"),
		})
	})

	err := installPluginChannels(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `channel plugin "serve-test-vanished"`)
	assert.Contains(t, err.Error(), "levee plugin disable serve-test-vanished")
}
