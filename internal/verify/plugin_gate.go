// plugin_gate.go implements the "plugin" gate kind: a verification check
// executed by a registered gate-type plugin sub-process. It is the second
// engine consumption point of the plugin system (notifier was the first).
//
// The gate is transport-agnostic in the framework's sense: it does not touch
// target channels — the plugin binary runs on the HOST and reports a verdict
// over the JSON-line stdio protocol. The plugin name and its parameters are
// declarations; the binary path and config come from a resolver the
// deployment supplies at materialisation time, exactly like the Channels
// provider or the Approver for the other gate kinds.
package verify

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/plugin"
)

// PluginSpec is the executable identity of one gate-type plugin.
type PluginSpec struct {
	Name       string
	BinaryPath string
	// Config is the plugin's stored configuration (parsed from the registry
	// record's config_yaml), sent in the init request so the plugin's own
	// install-time choices govern its check.
	Config map[string]any
}

// PluginResolver maps a plugin name to its executable spec. Implementations
// read the plugin registry; a name that does not resolve to an ENABLED gate
// plugin (or whose binary has vanished) is an error.
type PluginResolver func(pluginName string) (PluginSpec, error)

// PluginGate runs one gate-type plugin check. The zero value is not usable;
// construct with NewPluginGate.
type PluginGate struct {
	gateName   string
	phase      GatePhase
	pluginName string
	params     map[string]any
	timeout    *time.Duration
	resolver   PluginResolver
}

// NewPluginGate builds a plugin gate. Params and timeout come from the DSL
// declaration (passed through verbatim, like probe/slo/human); resolver is
// the deployment's plugin registry lookup. A nil resolver fails every Check
// rather than a construction error: the executability judgement at plan time
// already refuses declarations when no resolver is wired, and this path is
// reached only for a runtime whose registry disappeared between plan and
// run — a failure is the honest answer.
func NewPluginGate(name string, phase GatePhase, pluginName string, params map[string]any, timeout string, resolver PluginResolver) *PluginGate {
	g := &PluginGate{
		gateName:   name,
		phase:      phase,
		pluginName: pluginName,
		params:     params,
		resolver:   resolver,
	}
	if timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil && d > 0 {
			g.timeout = &d
		}
	}
	return g
}

// Name implements Gate.
func (g *PluginGate) Name() string { return g.gateName }

// Phase implements Gate.
func (g *PluginGate) Phase() GatePhase { return g.phase }

// Check spawns the plugin once and runs the declared check against it. The
// response contract is the example plugin's: init carries the stored
// config, check carries the declaration params (plus an injected
// timeout_seconds when the declaration set one), and the verdict is
// Result.Passed — a plugin that answers ok without a verdict is refused
// rather than read as a pass.
func (g *PluginGate) Check(ctx context.Context, _ GateInput) (GateResult, error) {
	if g.resolver == nil {
		return GateResult{}, fmt.Errorf("plugin gate %q: no plugin resolver (declared for plugin %q)", g.gateName, g.pluginName)
	}
	if g.pluginName == "" {
		return GateResult{}, fmt.Errorf("plugin gate %q: empty plugin name", g.gateName)
	}

	spec, err := g.resolver(g.pluginName)
	if err != nil {
		return GateResult{}, fmt.Errorf("plugin gate %q: resolve plugin %q: %w", g.gateName, g.pluginName, err)
	}

	checkParams := make(map[string]any, len(g.params)+1)
	for k, v := range g.params {
		checkParams[k] = v
	}
	if g.timeout != nil {
		checkParams["timeout_seconds"] = int(g.timeout.Seconds())
	}

	requests := []plugin.HostRequest{
		{Action: "init", Config: spec.Config},
		{Action: "check", Params: checkParams},
	}
	responses, err := plugin.Probe(ctx, spec.BinaryPath, requests)
	if err != nil {
		return GateResult{}, fmt.Errorf("plugin gate %q: plugin %q: %w", g.gateName, g.pluginName, err)
	}
	if len(responses) != 2 {
		return GateResult{}, fmt.Errorf("plugin gate %q: plugin %q returned %d responses, expected 2 (init + check)",
			g.gateName, g.pluginName, len(responses))
	}

	initResp := responses[0]
	if !initResp.OK {
		return GateResult{}, fmt.Errorf("plugin gate %q: plugin %q refused init: %s", g.gateName, g.pluginName, initResp.Error)
	}

	checkResp := responses[1]
	if !checkResp.OK {
		return GateResult{}, fmt.Errorf("plugin gate %q: plugin %q refused check: %s", g.gateName, g.pluginName, checkResp.Error)
	}
	if checkResp.Result == nil {
		// Fail closed: "ok: true" without a verdict is not a pass, it is a
		// malformed plugin response — the same class as a command gate
		// whose command printed nothing.
		return GateResult{}, fmt.Errorf("plugin gate %q: plugin %q returned ok without a verdict", g.gateName, g.pluginName)
	}

	return GateResult{
		Passed:  checkResp.Result.Passed,
		Message: checkResp.Result.Message,
		Details: checkResp.Result.Details,
	}, nil
}
