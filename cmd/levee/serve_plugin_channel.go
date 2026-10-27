// serve_plugin_channel.go wires enabled channel plugins into the process-wide
// channel registry — the serve half of the plugin system's fourth and last
// engine consumption point. After this, an inventory row whose `channel_type`
// names the plugin dials through it exactly like ssh or winrm, with no
// recompilation: the dynamic loading that docs/channel-plugins.md used to
// list as a V2 backlog item now exists for runtime plugins.
//
// The registry itself is the opt-in (install + enable), the same rule the
// other three tiers follow; no separate config switch is added on top.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/channel"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/plugin"
)

// installPluginChannels registers every ENABLED channel plugin into the
// process-wide channel registry, under the plugin's own name — one plugin,
// one transport type.
//
// Failure semantics mirror the other tiers: a missing registry file is the
// unconfigured state; a present registry that cannot be opened, an enabled
// channel whose binary is gone, and an enabled channel that fails its meta
// handshake all refuse the boot with the plugin named and the escape hatch
// printed. A plugin that would shadow an already-registered transport type
// (ssh, winrm, local, ...) is refused before registration — the registry's
// Register overwrites by design, and silently replacing "ssh" would route
// every workflow written against it through an operator's plugin.
func installPluginChannels(ctx context.Context, cfg *config.Config) error {
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
		if rec.Type != plugin.TypeChannel {
			continue // notify/gate/module surfaces are separate tiers
		}
		if _, exists := channel.DefaultRegistry().Factory(rec.Name); exists {
			return fmt.Errorf("serve: channel plugin %q collides with an already-registered channel type of the same name; rename the plugin or boot without it (`levee plugin disable %s`)",
				rec.Name, rec.Name)
		}
		f, err := plugin.NewPluginChannelFactory(ctx, rec)
		if err != nil {
			return fmt.Errorf("serve: channel plugin %q: %w (disable it with `levee plugin disable %s` to boot without it)",
				rec.Name, err, rec.Name)
		}
		channel.DefaultRegistry().Register(rec.Name, f)
		installed++
	}
	if installed > 0 {
		log.Info("serve: plugin channel transports installed", "count", installed)
	}
	return nil
}
