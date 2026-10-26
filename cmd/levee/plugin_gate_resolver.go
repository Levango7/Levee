// plugin_gate_resolver.go is the serve-side half of the plugin-check
// resolver: a declared plugin name is resolved against the plugin registry
// at check time (not at plan time — the registry can change between plan and
// run, and the check is where the plugin is actually executed).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/plugin"
	"github.com/nexus/levee/internal/verify"
)

// pluginGateResolverFromRegistry builds the verify.PluginResolver for this
// deployment: every resolution reads the registry fresh, so an operator who
// disables a plugin after planning gets the refusal at check time rather than
// a stale boot-time snapshot. A missing registry file means no plugin gates
// can resolve — the error names the reason rather than a bare lookup miss.
func pluginGateResolverFromRegistry(cfg *config.Config) verify.PluginResolver {
	registryPath := pluginRegistryPath(cfg)

	return func(pluginName string) (verify.PluginSpec, error) {
		if _, err := os.Stat(registryPath); err != nil {
			return verify.PluginSpec{}, fmt.Errorf("plugin registry %s is not available (install and enable a gate plugin first)", registryPath)
		}

		registry, err := plugin.NewRegistry(context.Background(), registryPath)
		if err != nil {
			return verify.PluginSpec{}, fmt.Errorf("open plugin registry: %w", err)
		}
		defer func() { _ = registry.Close() }()

		rec, err := registry.Get(context.Background(), pluginName)
		if err != nil {
			return verify.PluginSpec{}, fmt.Errorf("plugin %q: %w", pluginName, err)
		}
		if rec == nil {
			return verify.PluginSpec{}, fmt.Errorf("plugin %q is not registered (install it with `levee plugin install`)", pluginName)
		}
		if rec.Type != plugin.TypeGate {
			return verify.PluginSpec{}, fmt.Errorf("plugin %q is of type %q; only gate plugins can run plugin checks", pluginName, rec.Type)
		}
		if rec.State != plugin.StateEnabled {
			return verify.PluginSpec{}, fmt.Errorf("plugin %q is %s; enable it first (`levee plugin enable %s`)", pluginName, rec.State, pluginName)
		}
		if _, err := os.Stat(rec.BinaryPath); err != nil {
			return verify.PluginSpec{}, fmt.Errorf("plugin %q binary %s is gone: %w", pluginName, rec.BinaryPath, err)
		}

		spec := verify.PluginSpec{
			Name:       rec.Name,
			BinaryPath: rec.BinaryPath,
		}
		if rec.ConfigYAML != "" {
			cfgMap, err := plugin.ParseConfigYAML(rec.ConfigYAML)
			if err != nil {
				return verify.PluginSpec{}, fmt.Errorf("plugin %q stored config: %w", pluginName, err)
			}
			spec.Config = cfgMap
		}
		return spec, nil
	}
}
