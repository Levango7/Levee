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
// process-wide channel registry, under the plugin's own name AND every extra
// type it declared in channel_types (D-1c): one plugin, one or more transport
// types. An empty declaration reproduces the historical one-name behaviour, so
// existing manifests and registry rows are unaffected.
//
// Failure semantics mirror the other tiers: a missing registry file is the
// unconfigured state; a present registry that cannot be opened, an enabled
// channel whose binary is gone, and an enabled channel that fails its meta
// handshake all refuse the boot with the plugin named and the escape hatch
// printed. A plugin that would shadow an already-registered transport type
// (ssh, winrm, local, or another plugin's name/alias) is refused BEFORE any
// registration — the registry's Register overwrites by design, and silently
// replacing "ssh" would route every workflow written against it through an
// operator's plugin; a half-applied alias set would be worse still.
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
		types := pluginChannelTypeNames(rec)
		// Check every declared type before building anything: a collision must
		// leave the registry exactly as it was, not with the plugin half in.
		for _, typ := range types {
			if _, exists := channel.DefaultRegistry().Factory(typ); exists {
				return fmt.Errorf("serve: channel plugin %q would register channel type %q, which is already registered (a built-in transport or another plugin); rename the type or boot without it (`levee plugin disable %s`)",
					rec.Name, typ, rec.Name)
			}
		}
		f, err := plugin.NewPluginChannelFactory(ctx, rec)
		if err != nil {
			return fmt.Errorf("serve: channel plugin %q: %w (disable it with `levee plugin disable %s` to boot without it)",
				rec.Name, err, rec.Name)
		}
		for _, typ := range types {
			channel.DefaultRegistry().Register(typ, f)
			installed++
		}
	}
	if installed > 0 {
		log.Info("serve: plugin channel transports installed", "count", installed)
	}
	return nil
}

// pluginChannelTypeNames returns the channel type names a plugin should be
// registered under: its own name first, then each declared extra type, deduped
// and with empty entries dropped. The name is always included, which is what
// keeps an undeclared plugin (or an old registry row) serving exactly one type.
func pluginChannelTypeNames(rec *plugin.RegistryRecord) []string {
	types := []string{rec.Name}
	seen := map[string]bool{rec.Name: true}
	for _, t := range rec.ChannelTypes {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		types = append(types, t)
	}
	return types
}
