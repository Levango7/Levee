// Package plugin defines LEVEE's plugin system (design doc section 4.6, F03).
//
// LEVEE supports four categories of plugins that extend the four core
// orchestration abstractions without modifying the engine itself:
//
//   - ChannelPlugin    — extends the channel abstraction layer (CAL) with a
//     new transport (e.g. a vendor REST API, an agent protocol).
//   - GatePlugin       — extends the verification gate framework with a new
//     check (e.g. HTTP probe, DB query, custom metric).
//   - ModulePlugin     — extends the module executor with a new action module
//     (e.g. cloud-specific resource mutation).
//   - NotifierPlugin   — extends the notification framework with a new
//     delivery channel (e.g. Slack, PagerDuty).
//
// Every plugin implements the Plugin interface (Meta / Init / Close) plus
// exactly one of the four category interfaces. Plugins run in a separate
// sub-process managed by a Sandbox; the host and plugin communicate over
// gRPC and never share memory. The PluginManager owns the lifecycle
// (load → enable → disable → unload) and persists plugin metadata and
// state in a SQLite-backed Registry.
//
// The interfaces in this file are deliberately minimal and transport-agnostic
// so that a plugin compiled against this package can be loaded by any host
// version that respects the semver contract declared in PluginMeta.
package plugin

import (
	"context"
	"errors"
	"fmt"
)

// --- Plugin type & state ----------------------------------------------------

// PluginType identifies the category of a plugin. The string values are
// stable identifiers used in the registry, the CLI and the plugin manifest.
type PluginType string

const (
	// TypeChannel is the channel-extension plugin type. A ChannelPlugin
	// plugs a new transport into the channel abstraction layer.
	TypeChannel PluginType = "channel"

	// TypeGate is the gate-extension plugin type. A GatePlugin plugs a new
	// verification check into the gate framework.
	TypeGate PluginType = "gate"

	// TypeModule is the module-extension plugin type. A ModulePlugin plugs
	// a new action module into the executor.
	TypeModule PluginType = "module"

	// TypeNotifier is the notifier-extension plugin type. A NotifierPlugin
	// plugs a new delivery channel into the notification framework.
	TypeNotifier PluginType = "notify"
)

// AllTypes returns the four plugin types in canonical order. It is intended
// for iteration in diagnostics, the CLI and tests.
func AllTypes() []PluginType {
	return []PluginType{TypeChannel, TypeGate, TypeModule, TypeNotifier}
}

// Validate reports whether t is one of the four recognised plugin types.
func (t PluginType) Validate() bool {
	switch t {
	case TypeChannel, TypeGate, TypeModule, TypeNotifier:
		return true
	}
	return false
}

// PluginState is the lifecycle state of a plugin as recorded by the
// Registry. The values are stored in the plugin_registry.state column.
type PluginState string

const (
	// StateInstalled means the plugin binary has been registered but not
	// yet enabled. This is the initial state after a successful install.
	StateInstalled PluginState = "installed"

	// StateEnabled means the plugin is loaded and active: the manager has
	// started its sub-process and is ready to dispatch calls to it.
	StateEnabled PluginState = "enabled"

	// StateDisabled means the plugin was explicitly disabled by the
	// operator. Its sub-process is stopped but the binary remains
	// registered and can be re-enabled.
	StateDisabled PluginState = "disabled"

	// StateError means the plugin failed to load or crashed beyond the
	// sandbox restart budget. The operator must investigate and re-enable
	// it explicitly after remediation.
	StateError PluginState = "error"
)

// AllStates returns the four plugin states in canonical order.
func AllStates() []PluginState {
	return []PluginState{StateInstalled, StateEnabled, StateDisabled, StateError}
}

// --- Plugin metadata --------------------------------------------------------

// PluginMeta is the immutable metadata declared by every plugin. It is
// returned by Plugin.Meta() and persisted in the Registry. The host uses
// it for version-compatibility checks, display and audit.
type PluginMeta struct {
	// Name is the plugin's unique identifier, e.g. "http-probe". It is
	// used as the registry key and must match the directory name under
	// plugins/. Names must match ^[a-z][a-z0-9-]*$.
	Name string `json:"name" yaml:"name"`

	// Version is the plugin's semantic version, e.g. "1.2.3". The host
	// checks it against MinHostVersion / MaxHostVersion at load time.
	Version string `json:"version" yaml:"version"`

	// Type is the plugin category. It determines which of the four
	// category interfaces the plugin implements.
	Type PluginType `json:"type" yaml:"type"`

	// Author is a free-form contact string for the plugin maintainer.
	Author string `json:"author" yaml:"author"`

	// Description is a short human-readable summary shown by
	// `levee plugin list` / `levee plugin info`.
	Description string `json:"description" yaml:"description"`

	// MinHostVersion is the minimum LEVEE host version the plugin is
	// compatible with. Empty means "no lower bound".
	MinHostVersion string `json:"min_host_version,omitempty" yaml:"min_host_version,omitempty"`

	// MaxHostVersion is the maximum LEVEE host version the plugin is
	// compatible with. Empty means "no upper bound".
	MaxHostVersion string `json:"max_host_version,omitempty" yaml:"max_host_version,omitempty"`

	// EntryPoint is the relative path of the plugin binary inside the
	// plugin directory. Defaults to "plugin" when empty.
	EntryPoint string `json:"entry_point,omitempty" yaml:"entry_point,omitempty"`
}

// --- Plugin interface -------------------------------------------------------

// Plugin is the contract every LEVEE plugin must implement. It is the
// minimal lifecycle interface; category-specific behaviour is declared by
// additionally implementing one of ChannelPlugin / GatePlugin /
// ModulePlugin / NotifierPlugin.
//
// Implementations must be safe for concurrent use: the host may invoke
// methods on the same Plugin instance from multiple goroutines
// simultaneously.
type Plugin interface {
	// Meta returns the plugin's immutable metadata. It must return the
	// same value across the plugin's lifetime and must not perform I/O.
	Meta() PluginMeta

	// Init initialises the plugin with the host-supplied configuration.
	// The config map is loaded from the plugin's YAML config file and
	// contains only the plugin's own section; it never includes host
	// secrets or other plugins' configuration. Init is called exactly
	// once, before any category method.
	Init(config map[string]any) error

	// Close releases all resources held by the plugin. It is called once
	// when the plugin is disabled or the host is shutting down. Close must
	// be idempotent: calling it on an already-closed plugin returns nil.
	Close() error
}

// --- Category interfaces ----------------------------------------------------

// ChannelPlugin extends the channel abstraction layer with a new transport.
// A ChannelPlugin is the plugin-side counterpart of channel.Channel: the
// host marshals calls over gRPC and the plugin executes them against the
// target it owns.
//
// Implementations must respect ctx for cancellation and timeouts on every
// method. Plaintext credentials are never passed to a channel plugin; the
// host passes an opaque credential reference (e.g. a vault path) and the
// plugin resolves it through the host-supplied resolver if it needs the
// secret.
type ChannelPlugin interface {
	Plugin

	// Connect establishes the transport session to the target described
	// by targetRef. It must be safe to call on an already-connected
	// channel (no-op, returns nil).
	Connect(ctx context.Context, targetRef map[string]any) error

	// Exec runs a command on the target and returns its full result.
	// The result is marshalled back to the host over gRPC.
	Exec(ctx context.Context, cmd string) (ChannelExecResult, error)

	// Collect retrieves evidence from the target (e.g. file content,
	// process list). The returned bytes are the serialised evidence; the
	// host stores them in the audit trail.
	Collect(ctx context.Context, spec map[string]any) ([]byte, error)

	// Disconnect tears down the transport session. It is idempotent.
	Disconnect(ctx context.Context) error
}

// ChannelExecResult is the plugin-side counterpart of channel.ExecResult.
// It is marshalled over gRPC and reconstructed on the host side.
type ChannelExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration int64  `json:"duration_ms"` // milliseconds
}

// GatePlugin extends the verification gate framework with a new check.
// The host dispatches Check at the phase declared by Phase() and records
// the result in the audit trail.
type GatePlugin interface {
	Plugin

	// Name returns the gate's unique identifier. It must match
	// PluginMeta.Name so that the host can correlate the gate with its
	// registry entry.
	Name() string

	// Phase returns the phase at which this gate runs. The value must be
	// one of the four canonical phases defined in the verify package
	// ("pre_apply", "post_batch", "post_apply", "grace_period").
	Phase() string

	// Check runs the verification and returns its result. A nil error
	// with Passed == true means the gate passed; a nil error with
	// Passed == false means the gate ran successfully and the result is
	// a failure; a non-nil error means the gate could not run at all.
	Check(ctx context.Context, input map[string]any) (GateResult, error)
}

// GateResult is the plugin-side counterpart of verify.GateResult.
type GateResult struct {
	Passed  bool           `json:"passed"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Latency int64          `json:"latency_ms"`
}

// ModulePlugin extends the module executor with a new action module.
// The host dispatches Execute when a workflow step references the module
// by name.
type ModulePlugin interface {
	Plugin

	// Name returns the module's unique identifier. It must match
	// PluginMeta.Name.
	Name() string

	// Execute runs a single action on the target described by input.
	// The action verb is one of the actions declared by the module's
	// documentation; the host does not validate it before dispatch.
	Execute(ctx context.Context, action string, input map[string]any) (ModuleResult, error)

	// Validate checks that the given action and input are well-formed
	// before Execute is called. It returns nil when the input is valid,
	// or an error describing the problem. The host calls Validate once
	// per step before Execute.
	Validate(action string, input map[string]any) error
}

// ModuleResult is the plugin-side counterpart of executor.ModuleOutput.
type ModuleResult struct {
	ExitCode int            `json:"exit_code"`
	Stdout   string         `json:"stdout"`
	Stderr   string         `json:"stderr"`
	Changed  bool           `json:"changed"`
	Duration int64          `json:"duration_ms"`
	Details  map[string]any `json:"details,omitempty"`
}

// NotifierPlugin extends the notification framework with a new delivery
// channel. The host dispatches Send when a notification targets this
// channel.
type NotifierPlugin interface {
	Plugin

	// Name returns the notifier's unique identifier. It must match
	// PluginMeta.Name.
	Name() string

	// Send delivers the message to the channel's recipients. The message
	// is a structured payload; the plugin is responsible for formatting
	// it appropriately for the underlying transport (e.g. Slack webhook,
	// PagerDuty events API).
	Send(ctx context.Context, msg NotifyMessage) error
}

// NotifyMessage is the plugin-side counterpart of notify.Message. It
// carries everything a notifier needs to render and deliver a message.
type NotifyMessage struct {
	Event      string         `json:"event"` // e.g. "gate_failed", "run_completed"
	RunID      string         `json:"run_id"`
	Level      string         `json:"level"` // info|warning|error|critical
	Title      string         `json:"title"`
	Body       string         `json:"body"`
	Recipients []string       `json:"recipients"`
	Metadata   map[string]any `json:"metadata,omitempty"`
}

// --- Sentinel errors --------------------------------------------------------

// Sentinel errors returned by the plugin system. Callers may use errors.Is
// to test for them.
var (
	// ErrPluginNotFound is returned when an operation targets a plugin
	// that is not registered.
	ErrPluginNotFound = errors.New("plugin: not found")

	// ErrPluginExists is returned when attempting to register a plugin
	// whose name is already taken.
	ErrPluginExists = errors.New("plugin: already registered")

	// ErrPluginNotEnabled is returned when an operation requires an
	// enabled plugin but the plugin is not enabled.
	ErrPluginNotEnabled = errors.New("plugin: not enabled")

	// ErrPluginAlreadyEnabled is returned when enabling a plugin that is
	// already enabled.
	ErrPluginAlreadyEnabled = errors.New("plugin: already enabled")

	// ErrPluginAlreadyDisabled is returned when disabling a plugin that
	// is already disabled.
	ErrPluginAlreadyDisabled = errors.New("plugin: already disabled")

	// ErrInvalidPluginType is returned when a plugin declares an unknown
	// type.
	ErrInvalidPluginType = errors.New("plugin: invalid type")

	// ErrIncompatibleVersion is returned when a plugin's declared version
	// range does not include the running host version.
	ErrIncompatibleVersion = errors.New("plugin: incompatible version")

	// ErrSignatureMismatch is returned when plugin signature verification
	// fails.
	ErrSignatureMismatch = errors.New("plugin: signature mismatch")

	// ErrSandboxCrashed is returned when a plugin sub-process crashes
	// beyond the restart budget.
	ErrSandboxCrashed = errors.New("plugin: sandbox crashed")
)

// wrapError is a tiny helper that produces a wrapped error with the
// plugin name for context. It keeps error messages consistent across
// the package.
func wrapError(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("plugin %q: %w", name, err)
}
