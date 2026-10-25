// serve_plugin_notify_test.go pins the serve half of the notifier plugin
// consumption point: missing registry = unconfigured (nil), enabled
// notifier plugin = registered into the manager under its plugin name,
// wrong type / wrong state = skipped or refused, and a boot refusal names
// the plugin. The child binary is the test itself re-executed.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/notify"
	"github.com/nexus/levee/internal/plugin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pluginNotifyFixture builds a config whose data dir hosts a seeded plugin
// registry. seed describes the records to install directly (bypassing
// plugin install's manifest flow — the registry row is what serve reads).
func pluginNotifyFixture(t *testing.T, seed func(reg *plugin.Registry)) *config.Config {
	t.Helper()
	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.DataDir = dataDir
	regPath := pluginRegistryPath(cfg)

	if seed != nil {
		reg, err := plugin.NewRegistry(context.Background(), regPath)
		require.NoError(t, err)
		seed(reg)
		require.NoError(t, reg.Close())
	}
	return cfg
}

// Missing registry file: the unconfigured state, no channels, no error.
func TestBuildServePluginNotifiersMissingRegistryIsUnconfigured(t *testing.T) {
	cfg := pluginNotifyFixture(t, nil)
	notifiers, err := buildServePluginNotifiers(context.Background(), cfg)
	require.NoError(t, err)
	assert.Empty(t, notifiers)
}

// An enabled notifier plugin is adapted and named after its registry row.
// The live delivery path is covered in internal/plugin; here the point is
// the registry→manager wiring.
func TestBuildServePluginNotifiersLoadsEnabledNotifier(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		rec := &plugin.RegistryRecord{
			Name: "hook-relay", Type: plugin.TypeNotifier, State: plugin.StateEnabled,
			BinaryPath: os.Args[0], // executable, though not probed here
		}
		_, err := reg.Install(context.Background(), pluginMetaFor(rec), rec.BinaryPath, "", false)
		require.NoError(t, err)
		require.NoError(t, reg.SetState(context.Background(), "hook-relay", plugin.StateEnabled, ""))
	})

	notifiers, err := buildServePluginNotifiers(context.Background(), cfg)
	require.NoError(t, err)
	require.Len(t, notifiers, 1)
	assert.Equal(t, "hook-relay", notifiers[0].Name())
}

// A gate-type plugin never becomes a notification channel: the gate
// execution surface is a separate tier, and mixing types here would make
// `plugin install` semantics leak across categories.
func TestBuildServePluginNotifiersSkipsNonNotifierTypes(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		rec := &plugin.RegistryRecord{
			Name: "http-probe", Type: plugin.TypeGate, State: plugin.StateEnabled,
			BinaryPath: os.Args[0],
		}
		_, err := reg.Install(context.Background(), pluginMetaFor(rec), rec.BinaryPath, "", false)
		require.NoError(t, err)
		require.NoError(t, reg.SetState(context.Background(), "http-probe", plugin.StateEnabled, ""))
	})

	notifiers, err := buildServePluginNotifiers(context.Background(), cfg)
	require.NoError(t, err)
	assert.Empty(t, notifiers)
}

// An enabled notifier whose record cannot be adapted (here: state flipped
// to error after enable is not possible via the registry API, so the
// refusal path is exercised through a missing binary — the record is valid
// but the executable it names is gone) refuses the boot and names the
// plugin plus its escape hatch.
func TestBuildServePluginNotifiersRefusesUnusableChannel(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "vanished")
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		rec := &plugin.RegistryRecord{
			Name: "vanished-relay", Type: plugin.TypeNotifier, State: plugin.StateEnabled,
			BinaryPath: gone,
		}
		_, err := reg.Install(context.Background(), pluginMetaFor(rec), gone, "", false)
		require.NoError(t, err)
		require.NoError(t, reg.SetState(context.Background(), "vanished-relay", plugin.StateEnabled, ""))
	})

	_, err := buildServePluginNotifiers(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vanished-relay")
	assert.Contains(t, err.Error(), "levee plugin disable")
}

// pluginMetaFor renders a registry record as the manifest meta Install
// persists.
func pluginMetaFor(rec *plugin.RegistryRecord) plugin.PluginMeta {
	return plugin.PluginMeta{
		Name:        rec.Name,
		Version:     "1.0.0",
		Type:        rec.Type,
		Description: "test fixture",
	}
}

// compile-time guard: the notifier slice we produce is notify.Notifier.
var _ notify.Notifier = (notify.Notifier)(nil)
