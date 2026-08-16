// Plugin manager: orchestrates plugin lifecycle and dispatch.
//
// The PluginManager is the host-side facade that ties together the
// Registry (durable state), the Sandbox (sub-process isolation) and the
// in-memory plugin handle table. It is the single entry point that the
// rest of the engine uses to interact with plugins:
//
//   - Load / Install registers a plugin binary and records it in the
//     registry.
//   - Enable / Disable flip the plugin's state and start / stop the
//     sub-process sandbox.
//   - GetPlugin returns the in-memory handle for an enabled plugin so
//     that the caller can dispatch category-specific calls.
//   - ListPlugins returns the metadata of all installed plugins.
//
// Plugin configuration is loaded from a YAML file next to the binary
// (plugin.yaml) or from an explicit path supplied at install time. The
// config is stored in the registry so that the plugin can be re-enabled
// after a host restart without re-reading the file.
//
// The manager is safe for concurrent use. It does not load plugin
// binaries into the host's address space; all dispatch goes through the
// Sandbox, which communicates with the sub-process over gRPC.

package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"gopkg.in/yaml.v3"
)

// --- Manager configuration --------------------------------------------------

// ManagerConfig tunes the PluginManager.
type ManagerConfig struct {
	// PluginsDir is the root directory under which plugin binaries
	// live. Each plugin occupies a sub-directory named after the plugin
	// (e.g. /var/lib/levee/plugins/http-probe/).
	PluginsDir string `json:"plugins_dir" yaml:"plugins_dir"`

	// HostVersion is the running LEVEE host version, used for
	// compatibility checks. It should be set to the same value that
	// `levee version` reports.
	HostVersion string `json:"host_version" yaml:"host_version"`

	// Sandbox is the default sandbox configuration applied to every
	// plugin. Individual plugins can override it through their config.
	Sandbox SandboxConfig `json:"sandbox" yaml:"sandbox"`

	// VerifySignatures controls whether plugin signatures are verified
	// at install time. When true, the SHA-256 of the binary is recorded
	// in the registry and checked again at enable time.
	VerifySignatures bool `json:"verify_signatures" yaml:"verify_signatures"`

	// StopGrace is the grace period given to plugin sub-processes when
	// stopping them. Defaults to 5s.
	StopGrace time.Duration `json:"stop_grace" yaml:"stop_grace"`

	// OnCrash is the crash alert callback invoked when a plugin
	// sub-process crashes. It may be nil.
	OnCrash OnCrashFunc `json:"-"`
}

// DefaultManagerConfig returns a ManagerConfig with sensible defaults.
func DefaultManagerConfig() ManagerConfig {
	return ManagerConfig{
		PluginsDir:       "plugins",
		HostVersion:      "0.1.0",
		Sandbox:          DefaultSandboxConfig(),
		VerifySignatures: false,
		StopGrace:        5 * time.Second,
	}
}

// --- Plugin handle ----------------------------------------------------------

// handle is the in-memory representation of an enabled plugin. It ties
// the registry record to the running sandbox.
type handle struct {
	meta    PluginMeta
	sandbox *Sandbox
	config  map[string]any
}

// --- Manager ----------------------------------------------------------------

// PluginManager owns the plugin lifecycle. It is created once at host
// start-up and is safe for concurrent use.
type PluginManager struct {
	mu       sync.RWMutex
	registry *Registry
	cfg      ManagerConfig
	handles  map[string]*handle
}

// NewPluginManager creates a PluginManager backed by the given registry.
// The registry must already be open; the manager does not take ownership
// of closing it (the caller is responsible for closing both).
func NewPluginManager(registry *Registry, cfg ManagerConfig) *PluginManager {
	return &PluginManager{
		registry: registry,
		cfg:      cfg,
		handles:  make(map[string]*handle),
	}
}

// Registry returns the underlying registry. It is intended for CLI
// commands that need to inspect or mutate the registry directly.
func (m *PluginManager) Registry() *Registry { return m.registry }

// Config returns the manager configuration.
func (m *PluginManager) Config() ManagerConfig { return m.cfg }

// --- Install / Load ---------------------------------------------------------

// Install records a plugin binary in the registry and copies it into the
// plugins directory. The plugin's metadata is loaded from the manifest
// file (plugin.yaml) next to the binary. When verifySignatures is true
// the binary's SHA-256 is recorded for later verification.
//
// Install does not enable the plugin; call Enable separately.
func (m *PluginManager) Install(ctx context.Context, sourcePath string) (*RegistryRecord, error) {
	meta, binaryPath, configYAML, err := m.loadManifest(sourcePath)
	if err != nil {
		return nil, err
	}

	// Verify the binary exists.
	if _, err := os.Stat(binaryPath); err != nil {
		return nil, fmt.Errorf("plugin: stat binary %q: %w", binaryPath, err)
	}

	rec, err := m.registry.Install(ctx, meta, binaryPath, configYAML, m.cfg.VerifySignatures)
	if err != nil {
		return nil, err
	}
	log.Info("plugin installed",
		"name", meta.Name, "version", meta.Version, "type", meta.Type)
	return rec, nil
}

// Load is an alias for Install that keeps the interface documented in the
// task spec. It loads a plugin binary from path and records it in the
// registry without enabling it.
func (m *PluginManager) Load(ctx context.Context, path string) error {
	_, err := m.Install(ctx, path)
	return err
}

// loadManifest reads the plugin manifest (plugin.yaml) from the directory
// containing sourcePath (or from sourcePath itself when it is a
// directory) and returns the parsed metadata, the absolute binary path
// and the config YAML.
func (m *PluginManager) loadManifest(sourcePath string) (PluginMeta, string, string, error) {
	cleanPath := filepath.Clean(sourcePath)

	// Determine the plugin directory and binary path.
	pluginDir := cleanPath
	info, err := os.Stat(cleanPath)
	if err != nil {
		return PluginMeta{}, "", "", fmt.Errorf("plugin: stat %q: %w", cleanPath, err)
	}
	if !info.IsDir() {
		// sourcePath is a file; the plugin directory is its parent.
		pluginDir = filepath.Dir(cleanPath)
	}

	// Read the manifest.
	manifestPath := filepath.Join(pluginDir, "plugin.yaml")
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		return PluginMeta{}, "", "", fmt.Errorf("plugin: read manifest %q: %w", manifestPath, err)
	}

	var meta PluginMeta
	if err := yaml.Unmarshal(manifestData, &meta); err != nil {
		return PluginMeta{}, "", "", fmt.Errorf("plugin: parse manifest: %w", err)
	}
	if meta.Name == "" {
		return PluginMeta{}, "", "", fmt.Errorf("plugin: manifest missing name")
	}
	if !meta.Type.Validate() {
		return PluginMeta{}, "", "", fmt.Errorf("%w: %q", ErrInvalidPluginType, meta.Type)
	}

	// Resolve the binary path.
	entry := meta.EntryPoint
	if entry == "" {
		entry = "plugin"
	}
	binaryPath := filepath.Join(pluginDir, entry)
	if !info.IsDir() {
		// sourcePath was the binary itself; use it directly.
		binaryPath = cleanPath
	}

	// Read the optional config file (config.yaml). It is stored verbatim
	// in the registry; the plugin receives it as a map at Init time.
	configYAML := ""
	configPath := filepath.Join(pluginDir, "config.yaml")
	if data, err := os.ReadFile(configPath); err == nil {
		configYAML = string(data)
	}

	return meta, binaryPath, configYAML, nil
}

// --- Enable / Disable -------------------------------------------------------

// Enable starts the plugin sub-process and marks the plugin as enabled in
// the registry. It is a no-op (returns ErrPluginAlreadyEnabled) when the
// plugin is already enabled. The plugin's recorded config is parsed and
// will be passed to the plugin's Init when the sub-process connects.
func (m *PluginManager) Enable(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.handles[name]; ok {
		return fmt.Errorf("%w: %s", ErrPluginAlreadyEnabled, name)
	}

	rec, err := m.registry.Get(ctx, name)
	if err != nil {
		return err
	}

	if rec.State == StateEnabled {
		return fmt.Errorf("%w: %s", ErrPluginAlreadyEnabled, name)
	}

	// Version compatibility check.
	if !m.registry.IsCompatible(rec, m.cfg.HostVersion) {
		errMsg := fmt.Sprintf("incompatible: plugin range [%s, %s] excludes host %s",
			rec.MinHostVersion, rec.MaxHostVersion, m.cfg.HostVersion)
		_ = m.registry.SetState(ctx, name, StateError, errMsg)
		return fmt.Errorf("%w: %s", ErrIncompatibleVersion, errMsg)
	}

	// Signature verification (when recorded).
	if m.cfg.VerifySignatures && rec.Signature != "" {
		if err := m.registry.VerifySignature(ctx, name); err != nil {
			_ = m.registry.SetState(ctx, name, StateError, err.Error())
			return err
		}
	}

	// Parse config.
	config, _ := parseConfigYAML(rec.ConfigYAML)

	// Start the sandbox.
	sbCfg := m.cfg.Sandbox
	sb := NewSandbox(name, rec.BinaryPath, nil, nil, sbCfg, m.cfg.OnCrash)
	if err := sb.Start(ctx); err != nil {
		_ = m.registry.SetState(ctx, name, StateError, err.Error())
		return fmt.Errorf("plugin: start sandbox: %w", err)
	}

	if err := m.registry.SetState(ctx, name, StateEnabled, ""); err != nil {
		_ = sb.Stop(m.cfg.StopGrace)
		return err
	}

	m.handles[name] = &handle{
		meta: PluginMeta{
			Name:           rec.Name,
			Version:        rec.Version,
			Type:           rec.Type,
			Author:         rec.Author,
			Description:    rec.Description,
			MinHostVersion: rec.MinHostVersion,
			MaxHostVersion: rec.MaxHostVersion,
			EntryPoint:     rec.EntryPoint,
		},
		sandbox: sb,
		config:  config,
	}
	log.Info("plugin enabled", "name", name)
	return nil
}

// Disable stops the plugin sub-process and marks the plugin as disabled.
// It is a no-op (returns ErrPluginAlreadyDisabled) when the plugin is
// already disabled.
func (m *PluginManager) Disable(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	h, ok := m.handles[name]
	if !ok {
		// Not in memory; check the registry to distinguish "not found"
		// from "already disabled".
		rec, err := m.registry.Get(ctx, name)
		if err != nil {
			return err
		}
		if rec.State == StateDisabled || rec.State == StateInstalled {
			return fmt.Errorf("%w: %s", ErrPluginAlreadyDisabled, name)
		}
		// State is StateError or StateEnabled without a handle — just
		// flip the registry state.
		return m.registry.SetState(ctx, name, StateDisabled, "")
	}

	if err := h.sandbox.Stop(m.cfg.StopGrace); err != nil {
		log.Warn("sandbox stop failed during disable",
			"plugin", name, "err", err)
	}
	delete(m.handles, name)

	if err := m.registry.SetState(ctx, name, StateDisabled, ""); err != nil {
		return err
	}
	log.Info("plugin disabled", "name", name)
	return nil
}

// --- Remove -----------------------------------------------------------------

// Remove disables the plugin (if enabled) and deletes it from the
// registry. It does not delete the binary file; the caller is responsible
// for that.
func (m *PluginManager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	if h, ok := m.handles[name]; ok {
		_ = h.sandbox.Stop(m.cfg.StopGrace)
		delete(m.handles, name)
	}
	m.mu.Unlock()

	return m.registry.Remove(ctx, name)
}

// --- Query ------------------------------------------------------------------

// GetPlugin returns the in-memory handle for an enabled plugin. The
// returned PluginMeta can be used to dispatch category-specific calls
// through the sandbox's gRPC client. It returns ErrPluginNotEnabled when
// the plugin is not enabled.
func (m *PluginManager) GetPlugin(name string) (PluginMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	h, ok := m.handles[name]
	if !ok {
		return PluginMeta{}, fmt.Errorf("%w: %s", ErrPluginNotEnabled, name)
	}
	return h.meta, nil
}

// GetSandbox returns the sandbox for an enabled plugin. It is intended
// for advanced callers that need to interact with the sub-process
// directly (e.g. to attach a gRPC client).
func (m *PluginManager) GetSandbox(name string) (*Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	h, ok := m.handles[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPluginNotEnabled, name)
	}
	return h.sandbox, nil
}

// GetConfig returns the parsed configuration for an enabled plugin.
func (m *PluginManager) GetConfig(name string) (map[string]any, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	h, ok := m.handles[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPluginNotEnabled, name)
	}
	return h.config, nil
}

// ListPlugins returns the metadata of all installed plugins. It reads
// from the registry so that plugins that are installed but not enabled
// are also included.
func (m *PluginManager) ListPlugins(ctx context.Context) ([]PluginMeta, error) {
	records, err := m.registry.List(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]PluginMeta, 0, len(records))
	for _, rec := range records {
		out = append(out, PluginMeta{
			Name:           rec.Name,
			Version:        rec.Version,
			Type:           rec.Type,
			Author:         rec.Author,
			Description:    rec.Description,
			MinHostVersion: rec.MinHostVersion,
			MaxHostVersion: rec.MaxHostVersion,
			EntryPoint:     rec.EntryPoint,
		})
	}
	return out, nil
}

// ListRecords returns the full registry records (including state and
// config). It is intended for the CLI's `plugin list` / `plugin info`
// commands.
func (m *PluginManager) ListRecords(ctx context.Context) ([]*RegistryRecord, error) {
	return m.registry.List(ctx, "")
}

// Info returns the full registry record for a plugin. It is intended for
// the CLI's `plugin info <name>` command.
func (m *PluginManager) Info(ctx context.Context, name string) (*RegistryRecord, error) {
	return m.registry.Get(ctx, name)
}

// --- Shutdown ---------------------------------------------------------------

// Close disables all enabled plugins and stops their sandboxes. It is
// idempotent and safe to call from any goroutine. It does not close the
// underlying registry; the caller is responsible for that.
func (m *PluginManager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for name, h := range m.handles {
		if err := h.sandbox.Stop(m.cfg.StopGrace); err != nil {
			log.Warn("sandbox stop failed during shutdown",
				"plugin", name, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
		if err := m.registry.SetState(ctx, name, StateDisabled, ""); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	m.handles = make(map[string]*handle)
	return firstErr
}

// --- Helpers ----------------------------------------------------------------

// parseConfigYAML parses a YAML config blob into a map. It returns an
// empty map when the YAML is empty or cannot be parsed (config is
// best-effort; a malformed config should not prevent the plugin from
// loading).
func parseConfigYAML(s string) (map[string]any, error) {
	out := make(map[string]any)
	if s == "" {
		return out, nil
	}
	if err := yaml.Unmarshal([]byte(s), &out); err != nil {
		return out, err
	}
	return out, nil
}
