// plugin_irreversible.go supplies the plan-time irreversibility gate with the
// destructive actions that installed module-plugin manifests declared. It is
// the bridge between the registry (a stable install-time artifact) and
// wiring.WithInherentIrreversible, and it exists so the SAME declaration is
// read identically by `levee plan` / `levee apply` and by serve's PlanChange —
// the verdict is stamped into the plan and bound by plan_hash, so it must not
// depend on where the plan was produced.
//
// It must never execute a plugin: the declaration lives in the manifest, which
// install already read without running the binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/plugin"
)

// declaredPluginIrreversiblePairs returns the "<plugin>.<action>" pairs that
// enabled module-plugin manifests declared destructive, sorted, for
// wiring.WithInherentIrreversible.
//
// A missing registry file is the unconfigured state (no plugin was ever
// installed) and yields an empty list, not an error. A present-but-unreadable
// registry IS returned as an error and must fail the caller: silently dropping
// the declarations would judge a destructive plugin step reversible, and would
// produce a plan hash that differs from a deployment which could read them.
func declaredPluginIrreversiblePairs(ctx context.Context, cfg *config.Config) ([]string, error) {
	if cfg == nil {
		return nil, nil
	}
	regPath := pluginRegistryPath(cfg)
	if _, err := os.Stat(regPath); err != nil {
		return nil, nil
	}
	registry, err := plugin.NewRegistry(ctx, regPath)
	if err != nil {
		return nil, fmt.Errorf("plugin destructive-action vocabulary: open registry %q: %w", regPath, err)
	}
	defer func() { _ = registry.Close() }()

	records, err := registry.List(ctx, plugin.StateEnabled)
	if err != nil {
		return nil, fmt.Errorf("plugin destructive-action vocabulary: list enabled plugins: %w", err)
	}
	return plugin.DeclaredDestructivePairs(records), nil
}

// declaredPluginIrreversiblePairsForCmd is the compile-class convenience:
// `levee compile` / `new` / `import` build a dsl.Validator directly (no engine),
// and must feed it the same vocabulary serve and `levee plan` use, or the
// compile-time V14 gate and the plan-time LE082 gate would disagree about a
// plugin's destructive step. It loads the configuration itself because those
// commands did not previously need it.
func declaredPluginIrreversiblePairsForCmd() ([]string, error) {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return declaredPluginIrreversiblePairs(context.Background(), cfg)
}
