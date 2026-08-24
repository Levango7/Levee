// Package config provides configuration loading, validation and defaulting
// for the LEVEE engine. It uses viper to read YAML files and allows
// environment variable overrides with the LEVEE_ prefix.
//
// Configuration precedence (highest to lowest):
//  1. Environment variable (LEVEE_<SECTION>_<KEY>)
//  2. Config file value
//  3. Built-in default
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the root configuration object for LEVEE. It mirrors the
// structure of configs/config.yaml and is the single source of truth
// for all subsystem configuration.
type Config struct {
	Server     ServerConfig     `json:"server"     mapstructure:"server"`
	Database   DatabaseConfig   `json:"database"   mapstructure:"database"`
	Log        LogConfig        `json:"log"        mapstructure:"log"`
	Executor   ExecutorConfig   `json:"executor"   mapstructure:"executor"`
	Channel    ChannelConfig    `json:"channel"    mapstructure:"channel"`
	Approval   ApprovalConfig   `json:"approval"   mapstructure:"approval"`
	Audit      AuditConfig      `json:"audit"      mapstructure:"audit"`
	Lock       LockConfig       `json:"lock"       mapstructure:"lock"`
	Credential CredentialConfig `json:"credential" mapstructure:"credential"`
	Notify     NotifyConfig     `json:"notify"     mapstructure:"notify"`
	Permission PermissionConfig `json:"permission" mapstructure:"permission"`
	Verify     VerifyConfig     `json:"verify"     mapstructure:"verify"`
	Inventory  InventoryConfig  `json:"inventory"  mapstructure:"inventory"`
}

// ServerConfig holds server-mode runtime parameters.
type ServerConfig struct {
	DataDir   string `json:"data_dir"   mapstructure:"data_dir"`
	LogLevel  string `json:"log_level"  mapstructure:"log_level"`
	LogFormat string `json:"log_format" mapstructure:"log_format"`
}

// DatabaseConfig holds SQLite specific configuration. LEVEE MVP ships
// with a single embedded SQLite database file under Server.DataDir.
type DatabaseConfig struct {
	Driver string `json:"driver" mapstructure:"driver"` // sqlite (MVP)
	Path   string `json:"path"   mapstructure:"path"`   // absolute or relative db file path
	// Pool tuning
	MaxOpenConns    int           `json:"max_open_conns"    mapstructure:"max_open_conns"`
	MaxIdleConns    int           `json:"max_idle_conns"    mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `json:"conn_max_lifetime" mapstructure:"conn_max_lifetime"`
}

// LogConfig holds logging parameters. In the bundled config.yaml these
// fields live under `server`, but we expose them as a top-level section
// to make it easier to override via environment variables.
type LogConfig struct {
	Level  string `json:"level"  mapstructure:"level"`  // debug | info | warn | error
	Format string `json:"format" mapstructure:"format"` // text | json
	Output string `json:"output" mapstructure:"output"` // stdout | stderr | <path>
}

// ExecutorConfig holds the L2 orchestration layer tuning knobs.
type ExecutorConfig struct {
	DefaultConcurrency int             `json:"default_concurrency" mapstructure:"default_concurrency"`
	MaxConcurrency     int             `json:"max_concurrency"     mapstructure:"max_concurrency"`
	ConnectTimeout     time.Duration   `json:"connect_timeout"     mapstructure:"connect_timeout"`
	ExecTimeout        time.Duration   `json:"exec_timeout"        mapstructure:"exec_timeout"`
	RateLimit          RateLimitConfig `json:"rate_limit"          mapstructure:"rate_limit"`
}

// RateLimitConfig caps new connections per second at three granularities.
type RateLimitConfig struct {
	PerTarget  int `json:"per_target"  mapstructure:"per_target"`
	PerChannel int `json:"per_channel" mapstructure:"per_channel"`
	Global     int `json:"global"      mapstructure:"global"`
}

// ChannelConfig groups all channel implementations. Today SSH and WinRM
// are first-class; Agent / API / Interactive will be added in V1/V2.
type ChannelConfig struct {
	SSH   SSHConfig   `json:"ssh"   mapstructure:"ssh"`
	WinRM WinRMConfig `json:"winrm" mapstructure:"winrm"`
}

// SSHConfig configures the SSH channel.
type SSHConfig struct {
	Port            int           `json:"port"              mapstructure:"port"`
	AuthMethod      string        `json:"auth_method"       mapstructure:"auth_method"` // key | password
	KeyPath         string        `json:"key_path"          mapstructure:"key_path"`
	KnownHosts      string        `json:"known_hosts"       mapstructure:"known_hosts"`
	StrictHostCheck bool          `json:"strict_host_check" mapstructure:"strict_host_check"`
	ConnectTimeout  time.Duration `json:"connect_timeout"   mapstructure:"connect_timeout"`
	PoolSize        int           `json:"pool_size"         mapstructure:"pool_size"`
	// BecomeMethod enables privilege escalation for Exec: "" (default,
	// disabled) or "sudo". Any other value fails closed at Exec time.
	BecomeMethod string `json:"become_method" mapstructure:"become_method"`
	// BecomeUser is the escalation target when BecomeMethod=sudo; empty
	// means root. Requires passwordless (NOPASSWD) sudo on the target.
	BecomeUser string `json:"become_user" mapstructure:"become_user"`
}

// WinRMConfig configures the WinRM channel.
type WinRMConfig struct {
	Port           int           `json:"port"            mapstructure:"port"`
	Transport      string        `json:"transport"       mapstructure:"transport"` // negotiate | kerberos
	ConnectTimeout time.Duration `json:"connect_timeout" mapstructure:"connect_timeout"`
	PoolSize       int           `json:"pool_size"       mapstructure:"pool_size"`
}

// ApprovalConfig holds the three-tier approval timeouts.
type ApprovalConfig struct {
	StandardTimeout     time.Duration `json:"standard_timeout"     mapstructure:"standard_timeout"`
	HighTimeout         time.Duration `json:"high_timeout"         mapstructure:"high_timeout"`
	EmergencySupplement time.Duration `json:"emergency_supplement" mapstructure:"emergency_supplement"`
}

// AuditConfig controls the tamper-evident audit subsystem.
type AuditConfig struct {
	HashChain     bool   `json:"hash_chain"     mapstructure:"hash_chain"`
	WormStorage   bool   `json:"worm_storage"   mapstructure:"worm_storage"`
	RetentionDays int    `json:"retention_days" mapstructure:"retention_days"`
	ExportFormat  string `json:"export_format"  mapstructure:"export_format"`
}

// LockConfig tunes the distributed mutex used for target-level mutual exclusion.
type LockConfig struct {
	TTL           time.Duration `json:"ttl"            mapstructure:"ttl"`
	RetryInterval time.Duration `json:"retry_interval" mapstructure:"retry_interval"`
	MaxRetry      int           `json:"max_retry"      mapstructure:"max_retry"`
}

// CredentialConfig selects the credential storage backend and crypto parameters.
type CredentialConfig struct {
	Storage       string `json:"storage"        mapstructure:"storage"`        // local | vault
	Encryption    string `json:"encryption"     mapstructure:"encryption"`     // aes256-gcm
	KeyDerivation string `json:"key_derivation" mapstructure:"key_derivation"` // argon2id
}

// NotifyConfig configures outbound notifications. Today only webhook is supported.
type NotifyConfig struct {
	Webhook WebhookConfig `json:"webhook" mapstructure:"webhook"`
}

// WebhookConfig configures the webhook notifier.
type WebhookConfig struct {
	Enabled bool          `json:"enabled" mapstructure:"enabled"`
	URL     string        `json:"url"     mapstructure:"url"`
	Timeout time.Duration `json:"timeout" mapstructure:"timeout"`
	Retry   int           `json:"retry"   mapstructure:"retry"`
}

// PermissionConfig holds default permission namespace values.
type PermissionConfig struct {
	DefaultTeam string `json:"default_team" mapstructure:"default_team"`
	DefaultEnv  string `json:"default_env"  mapstructure:"default_env"`
}

// VerifyConfig holds settings for future verification / SLO gates.
type VerifyConfig struct {
	// PrometheusURL is the base URL of the Prometheus instance consulted by
	// future SLO gates. Empty disables those gates (MVP default).
	PrometheusURL string `json:"prometheus_url" mapstructure:"prometheus_url"`
}

// InventoryConfig holds inventory subsystem settings.
type InventoryConfig struct {
	// PatrolIntervalSeconds is the cadence, in seconds, of the future
	// reachability patrol loop over inventoried targets. 0 (the default)
	// disables the patrol.
	PatrolIntervalSeconds int `json:"patrol_interval_seconds" mapstructure:"patrol_interval_seconds"`
}

// Load reads the configuration file at path, applies env-var overrides
// (prefix LEVEE_, dots replaced by underscores, e.g. LEVEE_DATABASE_PATH),
// fills in defaults and validates the result. It returns a fully
// populated *Config or a wrapped error describing the failure.
//
// The caller is responsible for closing any file handles; Load does not
// keep the file open after returning.
func Load(path string) (*Config, error) {
	v := viper.New()

	// 1. Defaults — applied first so they can be overridden by file & env.
	setDefaults(v)

	// 2. File — optional in pure-env scenarios, but when path is given it must exist.
	if path != "" {
		if err := bindFile(v, path); err != nil {
			return nil, err
		}
	}

	// 3. Environment variables — highest precedence.
	bindEnv(v)

	if err := v.ReadInConfig(); err != nil {
		// If no config file was found but path was empty, that's OK —
		// we fall back to defaults + env. Any other error is fatal.
		var notFound viper.ConfigFileNotFoundError
		if path == "" && errors.As(err, &notFound) {
			// proceed with defaults + env
		} else if errors.As(err, &notFound) {
			return nil, fmt.Errorf("config file not found at %s: %w", path, err)
		} else {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	cfg := &Config{}
	// viper.Unmarshal already wires mapstructure.StringToTimeDurationHookFunc
	// and StringToSliceHookFunc by default, so we don't pass any extra
	// DecodeHook — adding a no-op hook would replace the default chain
	// and break duration parsing.
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	// Post-process: expand ~ in paths, derive database path if empty, etc.
	if err := postProcess(cfg); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate performs semantic validation that viper cannot express via
// struct tags: required fields, value ranges, enum membership, etc.
// It returns nil when the configuration is valid, or an error whose
// message lists every violation found (joined by '; ').
func Validate(cfg *Config) error { //nolint:gocyclo // inherently complex: validates 30+ fields with distinct rules per type
	var problems []string

	// Server
	if cfg.Server.DataDir == "" {
		problems = append(problems, "server.data_dir is required")
	}
	switch cfg.Server.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("server.log_level %q must be one of debug|info|warn|error", cfg.Server.LogLevel))
	}
	switch cfg.Server.LogFormat {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf("server.log_format %q must be one of text|json", cfg.Server.LogFormat))
	}

	// Database
	if cfg.Database.Path == "" {
		problems = append(problems, "database.path is required")
	}
	switch cfg.Database.Driver {
	case "sqlite":
	default:
		problems = append(problems, fmt.Sprintf("database.driver %q must be sqlite (MVP)", cfg.Database.Driver))
	}
	if cfg.Database.MaxOpenConns < 0 {
		problems = append(problems, "database.max_open_conns must be >= 0")
	}
	if cfg.Database.MaxIdleConns < 0 {
		problems = append(problems, "database.max_idle_conns must be >= 0")
	}
	if cfg.Database.MaxIdleConns > cfg.Database.MaxOpenConns && cfg.Database.MaxOpenConns > 0 {
		problems = append(problems, "database.max_idle_conns must be <= database.max_open_conns")
	}

	// Log
	switch cfg.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf("log.level %q must be one of debug|info|warn|error", cfg.Log.Level))
	}
	switch cfg.Log.Format {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf("log.format %q must be one of text|json", cfg.Log.Format))
	}

	// Executor
	if cfg.Executor.DefaultConcurrency <= 0 {
		problems = append(problems, "executor.default_concurrency must be > 0")
	}
	if cfg.Executor.MaxConcurrency <= 0 {
		problems = append(problems, "executor.max_concurrency must be > 0")
	}
	if cfg.Executor.DefaultConcurrency > cfg.Executor.MaxConcurrency {
		problems = append(problems, "executor.default_concurrency must be <= executor.max_concurrency")
	}
	if cfg.Executor.ConnectTimeout <= 0 {
		problems = append(problems, "executor.connect_timeout must be > 0")
	}
	if cfg.Executor.ExecTimeout <= 0 {
		problems = append(problems, "executor.exec_timeout must be > 0")
	}
	if cfg.Executor.RateLimit.PerTarget <= 0 {
		problems = append(problems, "executor.rate_limit.per_target must be > 0")
	}
	if cfg.Executor.RateLimit.PerChannel <= 0 {
		problems = append(problems, "executor.rate_limit.per_channel must be > 0")
	}
	if cfg.Executor.RateLimit.Global <= 0 {
		problems = append(problems, "executor.rate_limit.global must be > 0")
	}

	// Channel.SSH
	if cfg.Channel.SSH.Port <= 0 || cfg.Channel.SSH.Port > 65535 {
		problems = append(problems, "channel.ssh.port must be in 1..65535")
	}
	switch cfg.Channel.SSH.AuthMethod {
	case "key", "password":
	default:
		problems = append(problems, fmt.Sprintf("channel.ssh.auth_method %q must be key|password", cfg.Channel.SSH.AuthMethod))
	}
	if cfg.Channel.SSH.AuthMethod == "key" && cfg.Channel.SSH.KeyPath == "" {
		problems = append(problems, "channel.ssh.key_path is required when auth_method=key")
	}
	if cfg.Channel.SSH.ConnectTimeout <= 0 {
		problems = append(problems, "channel.ssh.connect_timeout must be > 0")
	}
	if cfg.Channel.SSH.PoolSize <= 0 {
		problems = append(problems, "channel.ssh.pool_size must be > 0")
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Channel.SSH.BecomeMethod)) {
	case "", "sudo":
	default:
		problems = append(problems, fmt.Sprintf("channel.ssh.become_method %q must be empty or sudo", cfg.Channel.SSH.BecomeMethod))
	}

	// Channel.WinRM
	if cfg.Channel.WinRM.Port <= 0 || cfg.Channel.WinRM.Port > 65535 {
		problems = append(problems, "channel.winrm.port must be in 1..65535")
	}
	switch cfg.Channel.WinRM.Transport {
	case "negotiate", "kerberos":
	default:
		problems = append(problems, fmt.Sprintf("channel.winrm.transport %q must be negotiate|kerberos", cfg.Channel.WinRM.Transport))
	}
	if cfg.Channel.WinRM.ConnectTimeout <= 0 {
		problems = append(problems, "channel.winrm.connect_timeout must be > 0")
	}
	if cfg.Channel.WinRM.PoolSize <= 0 {
		problems = append(problems, "channel.winrm.pool_size must be > 0")
	}

	// Approval
	if cfg.Approval.StandardTimeout <= 0 {
		problems = append(problems, "approval.standard_timeout must be > 0")
	}
	if cfg.Approval.HighTimeout <= 0 {
		problems = append(problems, "approval.high_timeout must be > 0")
	}
	if cfg.Approval.EmergencySupplement <= 0 {
		problems = append(problems, "approval.emergency_supplement must be > 0")
	}

	// Audit
	if cfg.Audit.RetentionDays <= 0 {
		problems = append(problems, "audit.retention_days must be > 0")
	}
	switch cfg.Audit.ExportFormat {
	case "json", "csv":
	default:
		problems = append(problems, fmt.Sprintf("audit.export_format %q must be json|csv", cfg.Audit.ExportFormat))
	}

	// Lock
	if cfg.Lock.TTL <= 0 {
		problems = append(problems, "lock.ttl must be > 0")
	}
	if cfg.Lock.RetryInterval <= 0 {
		problems = append(problems, "lock.retry_interval must be > 0")
	}
	if cfg.Lock.MaxRetry < 0 {
		problems = append(problems, "lock.max_retry must be >= 0")
	}

	// Credential
	switch cfg.Credential.Storage {
	case "local", "vault":
	default:
		problems = append(problems, fmt.Sprintf("credential.storage %q must be local|vault", cfg.Credential.Storage))
	}
	if cfg.Credential.Encryption == "" {
		problems = append(problems, "credential.encryption is required")
	}
	if cfg.Credential.KeyDerivation == "" {
		problems = append(problems, "credential.key_derivation is required")
	}

	// Notify.Webhook
	if cfg.Notify.Webhook.Enabled && cfg.Notify.Webhook.URL == "" {
		problems = append(problems, "notify.webhook.url is required when webhook is enabled")
	}
	if cfg.Notify.Webhook.Enabled && cfg.Notify.Webhook.Timeout <= 0 {
		problems = append(problems, "notify.webhook.timeout must be > 0 when webhook is enabled")
	}
	if cfg.Notify.Webhook.Retry < 0 {
		problems = append(problems, "notify.webhook.retry must be >= 0")
	}

	// Permission
	if cfg.Permission.DefaultTeam == "" {
		problems = append(problems, "permission.default_team is required")
	}
	if cfg.Permission.DefaultEnv == "" {
		problems = append(problems, "permission.default_env is required")
	}

	// Inventory
	if cfg.Inventory.PatrolIntervalSeconds < 0 {
		problems = append(problems, "inventory.patrol_interval_seconds must be >= 0 (0 disables the patrol)")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// setDefaults registers built-in defaults. They are the lowest-precedence
// source: any value present in the file or environment wins.
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.data_dir", "~/.levee/data")
	v.SetDefault("server.log_level", "info")
	v.SetDefault("server.log_format", "text")

	// Database (derived from server.data_dir in postProcess if empty)
	v.SetDefault("database.driver", "sqlite")
	v.SetDefault("database.max_open_conns", 50)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime", "30m")

	// Log (level/format mirror server.log_* by default via postProcess;
	// only output has a built-in default here).
	v.SetDefault("log.output", "stderr")

	// Executor
	v.SetDefault("executor.default_concurrency", 50)
	v.SetDefault("executor.max_concurrency", 200)
	v.SetDefault("executor.connect_timeout", "10s")
	v.SetDefault("executor.exec_timeout", "300s")
	v.SetDefault("executor.rate_limit.per_target", 5)
	v.SetDefault("executor.rate_limit.per_channel", 50)
	v.SetDefault("executor.rate_limit.global", 100)

	// Channel.SSH
	v.SetDefault("channel.ssh.port", 22)
	v.SetDefault("channel.ssh.auth_method", "key")
	v.SetDefault("channel.ssh.key_path", "~/.ssh/id_rsa")
	v.SetDefault("channel.ssh.known_hosts", "~/.ssh/known_hosts")
	v.SetDefault("channel.ssh.strict_host_check", true)
	v.SetDefault("channel.ssh.connect_timeout", "10s")
	v.SetDefault("channel.ssh.pool_size", 10)

	// Channel.WinRM
	v.SetDefault("channel.winrm.port", 5985)
	v.SetDefault("channel.winrm.transport", "negotiate")
	v.SetDefault("channel.winrm.connect_timeout", "10s")
	v.SetDefault("channel.winrm.pool_size", 5)

	// Approval
	v.SetDefault("approval.standard_timeout", "4h")
	v.SetDefault("approval.high_timeout", "8h")
	v.SetDefault("approval.emergency_supplement", "24h")

	// Audit
	v.SetDefault("audit.hash_chain", true)
	v.SetDefault("audit.worm_storage", true)
	v.SetDefault("audit.retention_days", 90)
	v.SetDefault("audit.export_format", "json")

	// Lock
	v.SetDefault("lock.ttl", "1h")
	v.SetDefault("lock.retry_interval", "5s")
	v.SetDefault("lock.max_retry", 10)

	// Credential
	v.SetDefault("credential.storage", "local")
	v.SetDefault("credential.encryption", "aes256-gcm")
	v.SetDefault("credential.key_derivation", "argon2id")

	// Notify.Webhook
	v.SetDefault("notify.webhook.enabled", false)
	v.SetDefault("notify.webhook.url", "")
	v.SetDefault("notify.webhook.timeout", "10s")
	v.SetDefault("notify.webhook.retry", 3)

	// Permission
	v.SetDefault("permission.default_team", "default")
	v.SetDefault("permission.default_env", "dev")

	// Verify
	v.SetDefault("verify.prometheus_url", "")

	// Inventory (0 disables the future reachability patrol loop)
	v.SetDefault("inventory.patrol_interval_seconds", 0)
}

// bindFile wires viper to the YAML file at path. The file extension is
// inferred from the path; only .yaml/.yml are supported in MVP.
func bindFile(v *viper.Viper, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve config path %q: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("config file not found at %q: %w", abs, err)
		}
		return fmt.Errorf("stat config file %q: %w", abs, err)
	}
	if info.IsDir() {
		return fmt.Errorf("config path %q is a directory, not a file", abs)
	}

	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(abs)), ".")
	switch ext {
	case "yaml", "yml":
		v.SetConfigType("yaml")
	default:
		return fmt.Errorf("unsupported config extension %q (want yaml or yml)", ext)
	}

	v.SetConfigFile(abs)
	return nil
}

// bindEnv registers every config key with an environment variable of the
// form LEVEE_<SECTION>_<KEY>. We use both AutomaticEnv (for ad-hoc Get
// calls) and explicit BindEnv for every known key — the latter is
// required because viper.Unmarshal walks AllSettings() which only
// surfaces env overrides for keys that have been explicitly bound.
func bindEnv(v *viper.Viper) {
	v.SetEnvPrefix("LEVEE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Explicitly bind every leaf key so Unmarshal picks up env values.
	for _, key := range allKeys() {
		_ = v.BindEnv(key)
	}
}

// allKeys returns the full list of dotted config keys that map to
// environment variables. Keep this in sync with the Config struct and
// setDefaults.
func allKeys() []string {
	return []string{
		"server.data_dir", "server.log_level", "server.log_format",
		"database.driver", "database.path",
		"database.max_open_conns", "database.max_idle_conns",
		"database.conn_max_lifetime",
		"log.level", "log.format", "log.output",
		"executor.default_concurrency", "executor.max_concurrency",
		"executor.connect_timeout", "executor.exec_timeout",
		"executor.rate_limit.per_target", "executor.rate_limit.per_channel",
		"executor.rate_limit.global",
		"channel.ssh.port", "channel.ssh.auth_method", "channel.ssh.key_path",
		"channel.ssh.known_hosts", "channel.ssh.strict_host_check",
		"channel.ssh.connect_timeout", "channel.ssh.pool_size",
		"channel.ssh.become_method", "channel.ssh.become_user",
		"channel.winrm.port", "channel.winrm.transport",
		"channel.winrm.connect_timeout", "channel.winrm.pool_size",
		"approval.standard_timeout", "approval.high_timeout",
		"approval.emergency_supplement",
		"audit.hash_chain", "audit.worm_storage",
		"audit.retention_days", "audit.export_format",
		"lock.ttl", "lock.retry_interval", "lock.max_retry",
		"credential.storage", "credential.encryption", "credential.key_derivation",
		"notify.webhook.enabled", "notify.webhook.url",
		"notify.webhook.timeout", "notify.webhook.retry",
		"permission.default_team", "permission.default_env",
		"verify.prometheus_url",
		"inventory.patrol_interval_seconds",
	}
}

// postProcess expands ~ in path fields and derives implicit values.
func postProcess(cfg *Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	cfg.Server.DataDir = expandHome(cfg.Server.DataDir, home)
	cfg.Channel.SSH.KeyPath = expandHome(cfg.Channel.SSH.KeyPath, home)
	cfg.Channel.SSH.KnownHosts = expandHome(cfg.Channel.SSH.KnownHosts, home)

	// Derive database path from data_dir when not explicitly set.
	if cfg.Database.Path == "" {
		cfg.Database.Path = filepath.Join(cfg.Server.DataDir, "levee.db")
	}
	cfg.Database.Path = expandHome(cfg.Database.Path, home)

	// Mirror server.log_* into log when log section was not explicitly
	// overridden — keeps both views consistent for callers.
	if cfg.Log.Level == "" {
		cfg.Log.Level = cfg.Server.LogLevel
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = cfg.Server.LogFormat
	}

	return nil
}

// expandHome replaces a leading ~ with the user's home dir.
// MVP only supports the current user; ~user is left untouched.
func expandHome(p, home string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "~\\") {
		return filepath.Join(home, p[2:])
	}
	return p
}
