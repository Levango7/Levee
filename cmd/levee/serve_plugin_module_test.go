// serve_plugin_module_test.go pins the serve half of the module plugin
// consumption point: missing registry = unconfigured (no-op), an enabled
// module plugin is registered into the process-wide executor under its plugin
// name and declares itself channel-free, other types are filtered out before
// any binary is touched, a name that would shadow a built-in module is
// refused, and an enabled module whose binary vanished refuses the boot with
// the escape hatch printed.
//
// The child binary is the test itself re-executed (the harness
// internal/plugin uses): TestMain routes the child persona for meta/close.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/plugin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveModuleChildEnv selects the plugin persona when the test binary is
// re-executed as a plugin child.
const serveModuleChildEnv = "LEVEE_SERVE_MODULE_TEST_CHILD"

func TestMain(m *testing.M) {
	if os.Getenv(serveModuleChildEnv) != "" {
		os.Exit(serveModuleChildMain())
	}
	os.Exit(m.Run())
}

// serveModuleChildMain answers the two verbs the boot path sends: the meta
// handshake (declaring one action and idempotency) and close.
func serveModuleChildMain() int {
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req plugin.HostRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return 1
		}
		switch req.Action {
		case "meta":
			_ = enc.Encode(plugin.PluginResponse{OK: true, Result: &plugin.ProbeResult{
				Passed:  true,
				Message: "serve-module-child",
				Details: map[string]any{
					"actions":    []any{"apply"},
					"idempotent": true,
				},
			}})
		case "close":
			_ = enc.Encode(plugin.PluginResponse{OK: true})
			return 0
		default:
			_ = enc.Encode(plugin.PluginResponse{OK: false, Error: "unknown action"})
		}
	}
	return 0
}

// seedEnabledPlugin writes one enabled registry row directly — the row is what
// serve reads; the manifest flow is covered by the plugin command tests.
func seedEnabledPlugin(t *testing.T, reg *plugin.Registry, rec *plugin.RegistryRecord) {
	t.Helper()
	rec.State = plugin.StateEnabled
	_, err := reg.Install(context.Background(), pluginMetaFor(rec), rec.BinaryPath, "", false)
	require.NoError(t, err)
	require.NoError(t, reg.SetState(context.Background(), rec.Name, plugin.StateEnabled, ""))
}

// No registry file: no plugin was ever installed, so there is nothing to do
// and nothing to complain about.
func TestInstallPluginModulesMissingRegistryIsNoop(t *testing.T) {
	cfg := pluginNotifyFixture(t, nil)

	require.NoError(t, installPluginModules(context.Background(), cfg))
}

// An enabled module plugin joins the process-wide executor under its registry
// name, with the surface its meta handshake declared, and without a channel.
func TestInstallPluginModulesRegistersEnabledModule(t *testing.T) {
	t.Setenv(serveModuleChildEnv, "1")
	const name = "serve-test-cloud-apply"
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name: name, Type: plugin.TypeModule, BinaryPath: os.Args[0],
		})
	})

	require.NoError(t, installPluginModules(context.Background(), cfg))
	t.Cleanup(func() { executor.DefaultExecutor().UnregisterModule(name) })

	m, ok := executor.DefaultExecutor().Module(name)
	require.True(t, ok, "the enabled module plugin must be dispatchable")
	assert.Equal(t, []string{"apply"}, m.Actions())
	assert.True(t, m.Idempotent())

	cf, ok := m.(executor.ChannelFree)
	require.True(t, ok, "a plugin module must declare itself channel-free")
	assert.False(t, cf.NeedsChannel())
}

// Types other than "module" are filtered out before their binary is even
// looked at: the binary here does not exist, so a probe would have failed.
func TestInstallPluginModulesSkipsOtherTypes(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name:       "serve-test-gate",
			Type:       plugin.TypeGate,
			BinaryPath: filepath.Join(t.TempDir(), "absent"),
		})
	})

	require.NoError(t, installPluginModules(context.Background(), cfg))
	_, ok := executor.DefaultExecutor().Module("serve-test-gate")
	assert.False(t, ok, "a gate plugin must never be registered as an executor module")
}

// A plugin whose name shadows a built-in module is refused before
// registration: RegisterModule overwrites by design, and silently replacing
// `shell` would swap a capability every workflow was written against.
func TestInstallPluginModulesRefusesBuiltinCollision(t *testing.T) {
	const builtin = "shell"
	require.True(t, func() bool {
		_, ok := executor.DefaultExecutor().Module(builtin)
		return ok
	}(), "precondition: the built-in shell module is registered in this binary")

	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name: builtin, Type: plugin.TypeModule, BinaryPath: os.Args[0],
		})
	})

	err := installPluginModules(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a built-in executor module")
	assert.Contains(t, err.Error(), "levee plugin disable shell")
}

// An enabled module whose binary vanished is unusable: the boot is refused
// with the plugin named and the escape hatch, not discovered on the first step
// hours later.
func TestInstallPluginModulesRefusesMissingBinary(t *testing.T) {
	cfg := pluginNotifyFixture(t, func(reg *plugin.Registry) {
		seedEnabledPlugin(t, reg, &plugin.RegistryRecord{
			Name:       "serve-test-vanished",
			Type:       plugin.TypeModule,
			BinaryPath: filepath.Join(t.TempDir(), "gone"),
		})
	})

	err := installPluginModules(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `module plugin "serve-test-vanished"`)
	assert.Contains(t, err.Error(), "levee plugin disable serve-test-vanished")
}
