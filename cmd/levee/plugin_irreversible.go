// plugin_irreversible.go supplies the irreversibility gates with the action
// classification that installed module-plugin manifests declared. It is the
// bridge between the registry (a stable install-time artifact) and three
// consumers: dsl.NewValidatorWithPluginPolicy (compile gate), wiring's
// WithPluginActionPolicy (plan-time gate), and conversation's
// PluginActionPolicy (draft-promotion gate). It exists so the SAME
// classification is read identically by `levee plan` / `levee apply` and by
// serve's PlanChange — the verdict is stamped into the plan and bound by
// plan_hash, so it must not depend on where the plan was produced.
//
// It must never execute a plugin: the declaration lives in the manifest, which
// install already read without running the binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/plugin"
)

// declaredPluginActionPolicy returns the installed module plugins' action
// classification — declared destructive pairs, declared reversible pairs, and
// the module names themselves (whose unclassified actions are judged
// irreversible) — for the compile, plan, and draft-promotion gates.
//
// A missing registry file is the unconfigured state (no plugin was ever
// installed) and yields the zero policy, not an error. A present-but-unreadable
// registry IS returned as an error and must fail the caller: silently dropping
// the declarations would judge a destructive plugin step reversible (or a
// reversible one irreversible), and would produce a plan hash that differs from
// a deployment which could read them.
func declaredPluginActionPolicy(ctx context.Context, cfg *config.Config) (dsl.PluginActionPolicy, error) {
	if cfg == nil {
		return dsl.PluginActionPolicy{}, nil
	}
	regPath := pluginRegistryPath(cfg)
	if _, err := os.Stat(regPath); err != nil {
		return dsl.PluginActionPolicy{}, nil
	}
	registry, err := plugin.NewRegistry(ctx, regPath)
	if err != nil {
		return dsl.PluginActionPolicy{}, fmt.Errorf("plugin action classification: open registry %q: %w", regPath, err)
	}
	defer func() { _ = registry.Close() }()

	records, err := registry.List(ctx, plugin.StateEnabled)
	if err != nil {
		return dsl.PluginActionPolicy{}, fmt.Errorf("plugin action classification: list enabled plugins: %w", err)
	}
	destructive, reversible, modules := plugin.DeclaredActionClassification(records)
	return dsl.NewPluginActionPolicy(destructive, reversible, modules), nil
}

// declaredPluginActionPolicyForCmd is the compile-class convenience:
// `levee compile` / `new` / `import` build a dsl.Validator directly (no engine),
// and must feed it the same classification serve and `levee plan` use, or the
// compile-time V14 gate and the plan-time LE082 gate would disagree about a
// plugin's action. It loads the configuration itself because those commands did
// not previously need it.
func declaredPluginActionPolicyForCmd() (dsl.PluginActionPolicy, error) {
	cfg, err := loadConfigForCmd()
	if err != nil {
		return dsl.PluginActionPolicy{}, fmt.Errorf("load config: %w", err)
	}
	return declaredPluginActionPolicy(context.Background(), cfg)
}
