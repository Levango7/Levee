// serve_plugin_notify.go wires enabled notifier plugins into the serving
// notification manager — the serve half of the plugin system's first
// engine consumption point. The registry itself is the opt-in: a plugin is
// here because an operator installed and enabled it, so no extra config
// switch is added on top (the same reasoning as alert-sources.yaml: the
// admin-managed file IS the switch).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/notify"
	"github.com/nexus/levee/internal/plugin"
)

// buildServePluginNotifiers loads every ENABLED notifier plugin from the
// registry and adapts it to a notify.Notifier. The semantics follow the
// webhook channel's rule: an enabled-but-unusable plugin refuses the boot
// (named), because a channel the operator enabled must not silently stop
// delivering.
//
// A missing registry file is the unconfigured state (no plugin was ever
// installed): nil, nil — same posture as a missing alert-sources.yaml. A
// registry that exists but cannot be opened refuses the boot: corruption
// discovered by silently skipping would strand deliveries with no word of
// why.
func buildServePluginNotifiers(ctx context.Context, cfg *config.Config) ([]notify.Notifier, error) {
	regPath := pluginRegistryPath(cfg)
	if _, err := os.Stat(regPath); err != nil {
		return nil, nil
	}

	registry, err := plugin.NewRegistry(ctx, regPath)
	if err != nil {
		return nil, fmt.Errorf("serve: open plugin registry: %w", err)
	}
	defer func() { _ = registry.Close() }()

	records, err := registry.List(ctx, plugin.StateEnabled)
	if err != nil {
		return nil, fmt.Errorf("serve: list enabled plugins: %w", err)
	}

	var notifiers []notify.Notifier
	for _, rec := range records {
		if rec.Type != plugin.TypeNotifier {
			continue // gate/module/channel execution surfaces are separate tiers
		}
		// Boot-time existence check: an enabled channel whose binary has
		// vanished is unusable, and the webhook rule applies — refuse the
		// boot with the name, don't discover it from the first missed
		// delivery. (Per-send failures after boot remain per-delivery
		// errors; this gate only answers "is the channel here at all".)
		if _, err := os.Stat(rec.BinaryPath); err != nil {
			return nil, fmt.Errorf("serve: plugin notifier %q: binary %q is gone: %w (disable it with `levee plugin disable %s` to boot without it)",
				rec.Name, rec.BinaryPath, err, rec.Name)
		}
		n, err := plugin.NewPluginNotifier(rec)
		if err != nil {
			return nil, fmt.Errorf("serve: plugin notifier %q: %w (disable it with `levee plugin disable %s` to boot without it)",
				rec.Name, err, rec.Name)
		}
		notifiers = append(notifiers, n)
	}
	return notifiers, nil
}

// installPluginNotifiers is the buildServeServices seam (kept out of that
// function, whose cyclomatic budget the main flow already spends): it loads
// the enabled notifier plugins and registers them into mgr — creating the
// manager when a plugin exists but the webhook does not, so plugin channels
// alone can justify a notification manager. Returns the manager to keep
// using (unchanged or newly created).
func installPluginNotifiers(ctx context.Context, cfg *config.Config, mgr *notify.NotificationManager) (*notify.NotificationManager, error) {
	notifiers, err := buildServePluginNotifiers(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if len(notifiers) == 0 {
		return mgr, nil
	}
	if mgr == nil {
		mgr = notify.NewNotificationManager()
	}
	for _, n := range notifiers {
		if err := mgr.Register(n); err != nil {
			return nil, fmt.Errorf("serve: register plugin notifier %q: %w", n.Name(), err)
		}
	}
	log.Info("serve: plugin notification channels installed", "count", len(notifiers))
	return mgr, nil
}

// installPluginExecutionSurfaces installs every plugin consumption point that
// is active at boot: notifier plugins into the notification manager (returned,
// possibly newly created), module plugins into the process-wide executor, and
// channel plugins into the process-wide channel registry.
//
// It exists as one call because buildServeServices' cyclomatic budget is
// already spent — three calls with three error branches would push it over the
// lint gate. Each half keeps its own failure semantics (all three refuse the
// boot for an enabled-but-unusable plugin, named, with the disable escape
// hatch), so this is a seam, not a merge.
func installPluginExecutionSurfaces(ctx context.Context, cfg *config.Config, mgr *notify.NotificationManager) (*notify.NotificationManager, error) {
	mgr, err := installPluginNotifiers(ctx, cfg, mgr)
	if err != nil {
		return nil, err
	}
	if err := installPluginModules(ctx, cfg); err != nil {
		return nil, err
	}
	if err := installPluginChannels(ctx, cfg); err != nil {
		return nil, err
	}
	return mgr, nil
}
