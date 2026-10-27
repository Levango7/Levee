// serve_plugin_module.go wires enabled module plugins into the process-wide
// executor — the serve half of the plugin system's third engine consumption
// point. The registry itself is the opt-in (install + enable), the same rule
// the notifier channels follow; no separate config switch is added on top.
//
// Registration happens once at boot because the executor is a static
// registry: unlike the gate resolver (which re-reads the registry per check)
// a module cannot appear mid-run — a step dispatched against a module that is
// not registered fails as an unknown module.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/executor"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plugin"
)

// installPluginModules registers every ENABLED module plugin into the
// process-wide executor, so a workflow step whose `module:` names the plugin
// dispatches to it exactly like a built-in module.
//
// Failure semantics mirror the notifier path: a missing registry file is the
// unconfigured state (no plugin was ever installed); a present registry that
// cannot be opened, an enabled module whose binary is gone, and an enabled
// module that fails its meta handshake all refuse the boot with the plugin
// named and the escape hatch printed — an operator-enabled capability must
// not silently stop existing.
func installPluginModules(ctx context.Context, cfg *config.Config) error {
	regPath := pluginRegistryPath(cfg)
	if _, err := os.Stat(regPath); err != nil {
		return nil
	}

	registry, err := plugin.NewRegistry(ctx, regPath)
	if err != nil {
		return fmt.Errorf("serve: open plugin registry: %w", err)
	}
	defer func() { _ = registry.Close() }()

	records, err := registry.List(ctx, plugin.StateEnabled)
	if err != nil {
		return fmt.Errorf("serve: list enabled plugins: %w", err)
	}

	installed := 0
	for _, rec := range records {
		if rec.Type != plugin.TypeModule {
			continue // channel/gate/notify surfaces are separate tiers
		}
		// A plugin must never shadow a built-in module. RegisterModule
		// overwrites by design (tests swap modules that way), so the
		// collision check lives here, before registration: silently
		// replacing `shell` with a plugin would swap a capability the
		// workflows were written against.
		if _, exists := executor.DefaultExecutor().Module(rec.Name); exists {
			return fmt.Errorf("serve: module plugin %q collides with a built-in executor module of the same name; rename the plugin or boot without it (`levee plugin disable %s`)",
				rec.Name, rec.Name)
		}
		m, err := plugin.NewPluginModule(ctx, rec)
		if err != nil {
			return fmt.Errorf("serve: module plugin %q: %w (disable it with `levee plugin disable %s` to boot without it)",
				rec.Name, err, rec.Name)
		}
		executor.DefaultExecutor().RegisterModule(m)
		installed++
	}
	if installed > 0 {
		log.Info("serve: plugin executor modules installed", "count", installed)
	}
	return nil
}
