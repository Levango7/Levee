// module.go is the execution half of the module plugin type: it adapts an
// enabled registry record (TypeModule) into an executor.Module, so a workflow
// step whose `module:` names the plugin is dispatched to the plugin binary
// exactly like a built-in module. One invocation = one short-lived process
// speaking the JSON-line protocol: init (the config captured at install
// time), validate (the host refuses a malformed step before it mutates
// anything), execute (the action itself), close.
//
// The module is declared channel-free: it drives its own target (a vendor
// API, a cloud control plane) rather than the SSH/WinRM session the step's
// inventory row would dial, so the dispatcher does not open one on its
// behalf. See executor.ChannelFree.
package plugin

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/nexus/levee/internal/executor"
)

// PluginModule adapts a module plugin to the executor's Module interface.
// The zero value is not usable; construct with NewPluginModule.
type PluginModule struct {
	name        string
	binaryPath  string
	configYAML  string
	actions     []string
	idempotent  bool
	destructive []string
}

// NewPluginModule builds the adapter and performs one meta handshake so the
// executor can validate actions before dispatch. The handshake is the same
// boot-time usability question the notifier path asks: an enabled module that
// cannot identify itself is unusable, and the caller refuses the boot with
// this error rather than discovering it on the first step.
//
// The declared action list is required — a module that declares none accepts
// no step, so accepting the record would register a capability that always
// fails. Idempotency defaults to false when undeclared: retry logic must
// never assume idempotency a module did not claim (executor.IsIdempotent's
// documented stance).
func NewPluginModule(ctx context.Context, rec *RegistryRecord) (*PluginModule, error) {
	if rec == nil {
		return nil, fmt.Errorf("plugin: module: nil registry record")
	}
	if rec.Type != TypeModule {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record is %q, want %q", ErrInvalidPluginType, rec.Type, TypeModule))
	}
	if rec.State != StateEnabled {
		return nil, wrapError(rec.Name, fmt.Errorf("%w: record state is %q", ErrPluginNotEnabled, rec.State))
	}

	responses, err := Probe(ctx, rec.BinaryPath, []HostRequest{{Action: "meta"}})
	if err != nil {
		return nil, wrapError(rec.Name, fmt.Errorf("meta handshake: %w", err))
	}
	if len(responses) != 1 {
		return nil, wrapError(rec.Name, fmt.Errorf("meta handshake returned %d responses, want 1", len(responses)))
	}
	actions, idempotent, err := declaredModuleSurface(rec.Name, responses[0])
	if err != nil {
		return nil, err
	}
	destructive, err := declaredDestructiveActions(rec.Name, rec.DestructiveActions, actions)
	if err != nil {
		return nil, err
	}

	return &PluginModule{
		name:        rec.Name,
		binaryPath:  rec.BinaryPath,
		configYAML:  rec.ConfigYAML,
		actions:     actions,
		idempotent:  idempotent,
		destructive: destructive,
	}, nil
}

// declaredDestructiveActions cross-checks the manifest's destructive_actions
// against the actions the plugin actually declares in its meta handshake. A
// pair the handshake does not offer can never run, so accepting it would let a
// manifest claim a destructive action that the plugin does not implement —
// and, worse, would put a phantom pair into the irreversibility vocabulary.
// The declaration is returned sorted and de-duplicated so the plan-time
// vocabulary is deterministic regardless of manifest order.
func declaredDestructiveActions(name string, declared, actions []string) ([]string, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	known := make(map[string]struct{}, len(actions))
	for _, a := range actions {
		known[a] = struct{}{}
	}
	seen := make(map[string]struct{}, len(declared))
	out := make([]string, 0, len(declared))
	for _, a := range declared {
		if _, ok := known[a]; !ok {
			return nil, wrapError(name, fmt.Errorf(
				"manifest destructive_actions names %q, which the plugin's meta handshake does not declare among its actions %v",
				a, actions))
		}
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// DestructiveActions returns the plugin's declared inherently-destructive
// actions (a subset of Actions()), sorted. It is empty when the manifest
// declared none.
func (m *PluginModule) DestructiveActions() []string {
	return append([]string(nil), m.destructive...)
}

// DeclaredDestructivePairs returns the "<plugin>.<action>" pairs declared as
// inherently destructive by every ENABLED module plugin in records, sorted.
// This is the vocabulary the plan-time irreversibility gate adds on top of
// dsl.DefaultIrreversibleActions: it is read from the registry (a stable
// install-time fact), never by executing a plugin, so the verdict — and the
// plan hash that carries it — is the same wherever the plan is produced.
func DeclaredDestructivePairs(records []*RegistryRecord) []string {
	var out []string
	for _, rec := range records {
		if rec == nil || rec.Type != TypeModule || rec.State != StateEnabled {
			continue
		}
		for _, action := range rec.DestructiveActions {
			out = append(out, rec.Name+"."+action)
		}
	}
	sort.Strings(out)
	return out
}

// declaredModuleSurface reads the module declaration out of a meta response.
// The field names live in the response's Details map, the same place the
// protocol already carries plugin identity (examples/plugins/http_probe).
func declaredModuleSurface(name string, resp PluginResponse) ([]string, bool, error) {
	if !resp.OK {
		return nil, false, wrapError(name, fmt.Errorf("meta refused: %s", resp.Error))
	}
	if resp.Result == nil {
		return nil, false, wrapError(name, fmt.Errorf("meta response carries no result block"))
	}

	raw, ok := resp.Result.Details["actions"]
	if !ok {
		return nil, false, wrapError(name, fmt.Errorf("meta declares no actions; a module plugin must list the verbs it accepts, e.g. details.actions: [\"apply\"]"))
	}
	actions, err := stringList(raw)
	if err != nil {
		return nil, false, wrapError(name, fmt.Errorf("meta details.actions: %w", err))
	}
	if len(actions) == 0 {
		return nil, false, wrapError(name, fmt.Errorf("meta declares an empty action list; a module that accepts no verb cannot run a step"))
	}

	idempotent := false
	if rawID, ok := resp.Result.Details["idempotent"]; ok {
		b, isBool := rawID.(bool)
		if !isBool {
			return nil, false, wrapError(name, fmt.Errorf("meta details.idempotent is %T, want bool", rawID))
		}
		idempotent = b
	}
	return actions, idempotent, nil
}

// stringList converts a decoded JSON array ([]any of strings, or a []string
// supplied programmatically) into a []string, rejecting mixed types rather
// than stringifying them into a plausible-looking verb set.
func stringList(raw any) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...), nil
	case []any:
		out := make([]string, 0, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("element %d is %T, want string", i, item)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%T is not a list", raw)
	}
}

// Name implements executor.Module: the plugin's registry name is the module
// name a workflow step declares.
func (m *PluginModule) Name() string { return m.name }

// Actions implements executor.Module with the verbs declared in the meta
// handshake, so an unknown verb is rejected before a process is started.
func (m *PluginModule) Actions() []string { return append([]string(nil), m.actions...) }

// Idempotent implements executor.Module with the declaration from meta.
func (m *PluginModule) Idempotent() bool { return m.idempotent }

// NeedsChannel implements executor.ChannelFree: the plugin owns its transport
// (a vendor API, a control plane), so no SSH/WinRM session is dialled on its
// behalf. The step's inventory target is still passed to the plugin as
// addressing, not as a connection.
func (m *PluginModule) NeedsChannel() bool { return false }

// Execute runs one action: spawn, init (registry config), validate, execute,
// close. A plugin refusal (ok:false) or protocol breach surfaces as the
// error; an execute answer that carries no module result is refused — ok:true
// with nothing in it is not a verdict.
func (m *PluginModule) Execute(ctx context.Context, action string, input executor.ModuleInput) (*executor.ModuleOutput, error) {
	bin, err := resolvePluginBinary(m.binaryPath)
	if err != nil {
		return nil, wrapError(m.name, err)
	}

	execCtx, cancel := context.WithTimeout(ctx, moduleDeadline)
	defer cancel()

	proc, err := startPluginProc(execCtx, bin)
	if err != nil {
		return nil, wrapError(m.name, err)
	}
	defer proc.close()

	// 1. init: hand over the config captured at install time, so operator
	//    choices (endpoint, credential reference, region) govern the
	//    invocation without being restated on every step. It is sent even
	//    when empty: the declared Plugin interface calls Init exactly once
	//    before any category method, so a plugin can depend on it.
	cfg := map[string]any{}
	if m.configYAML != "" {
		parsed, err := parseConfigYAML(m.configYAML)
		if err != nil {
			return nil, wrapError(m.name, fmt.Errorf("stored config: %w", err))
		}
		cfg = parsed
	}
	if resp, err := proc.request(HostRequest{Action: "init", Config: cfg}); err != nil {
		return nil, wrapError(m.name, err)
	} else if !resp.OK {
		return nil, wrapError(m.name, fmt.Errorf("init refused: %s", resp.Error))
	}

	// 2. validate: the plugin checks the step before anything is mutated.
	//    The declared interface makes Validate mandatory, so a plugin that
	//    refuses this verb is refused here rather than silently skipped.
	params := moduleParams(action, input)
	resp, err := proc.request(HostRequest{Action: "validate", Params: params})
	if err != nil {
		return nil, wrapError(m.name, err)
	}
	if !resp.OK {
		return nil, wrapError(m.name, fmt.Errorf("validate refused action %q: %s", action, resp.Error))
	}

	// 3. execute.
	resp, err = proc.request(HostRequest{Action: "execute", Params: params})
	if err != nil {
		return nil, wrapError(m.name, err)
	}
	if !resp.OK {
		return nil, wrapError(m.name, fmt.Errorf("execute refused action %q: %s", action, resp.Error))
	}
	if resp.Module == nil {
		return nil, wrapError(m.name, fmt.Errorf("execute action %q returned ok with no module result; an empty success is not a verdict", action))
	}

	return &executor.ModuleOutput{
		ExitCode: resp.Module.ExitCode,
		Stdout:   resp.Module.Stdout,
		Stderr:   resp.Module.Stderr,
		Changed:  resp.Module.Changed,
		Duration: time.Duration(resp.Module.Duration) * time.Millisecond,
	}, nil
}

// moduleParams maps one step invocation onto the wire shape of
// plugin.ModulePlugin.Execute (types.go): the verb, the step args, and the
// target as addressing data the plugin may use for logging or routing.
func moduleParams(action string, input executor.ModuleInput) map[string]any {
	args := input.Args
	if args == nil {
		args = map[string]any{}
	}
	params := map[string]any{
		"action": action,
		"args":   args,
	}
	if input.Target != nil {
		params["target"] = map[string]any{
			"host": input.Target.Host(),
			"port": input.Target.Port(),
			"type": input.Target.Type(),
		}
	}
	return params
}
