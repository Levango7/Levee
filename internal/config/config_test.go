package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeYAML is a tiny helper that writes content to a temp YAML file
// and returns its absolute path. The test is marked as failed on any
// IO error.
func writeYAML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

// minimalValidYAML returns a YAML document that exercises every
// section with legal values. It is intentionally explicit (no reliance
// on defaults) so we can detect regressions when defaults change.
func minimalValidYAML() string {
	return `# minimal valid config for tests
server:
  data_dir: /tmp/levee/data
  log_level: info
  log_format: text

database:
  driver: sqlite
  path: /tmp/levee/data/levee.db
  max_open_conns: 20
  max_idle_conns: 5
  conn_max_lifetime: 10m

log:
  level: debug
  format: json
  output: stdout

executor:
  default_concurrency: 10
  max_concurrency: 50
  connect_timeout: 5s
  exec_timeout: 60s
  rate_limit:
    per_target: 2
    per_channel: 10
    global: 20

channel:
  ssh:
    port: 22
    auth_method: key
    key_path: /tmp/levee/id_rsa
    known_hosts: /tmp/levee/known_hosts
    strict_host_check: true
    connect_timeout: 5s
    pool_size: 4
  winrm:
    port: 5985
    transport: negotiate
    connect_timeout: 5s
    pool_size: 2

approval:
  standard_timeout: 2h
  high_timeout: 4h
  emergency_supplement: 12h

audit:
  hash_chain: true
  worm_storage: false
  retention_days: 30
  export_format: json

lock:
  ttl: 30m
  retry_interval: 2s
  max_retry: 5

credential:
  storage: local
  encryption: aes256-gcm
  key_derivation: argon2id

notify:
  webhook:
    enabled: false
    url: ""
    timeout: 5s
    retry: 2

permission:
  default_team: ops
  default_env: staging
`
}

// withEnv sets env vars for the duration of the test and restores the
// originals on cleanup. Keys must already be in final form (LEVEE_...).
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		old, had := os.LookupEnv(k)
		require.NoError(t, os.Setenv(k, v))
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

func TestLoad_ValidFile(t *testing.T) {
	p := writeYAML(t, minimalValidYAML())
	cfg, err := Load(p)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Spot-check a few fields across sections.
	assert.Equal(t, "/tmp/levee/data", cfg.Server.DataDir)
	assert.Equal(t, "info", cfg.Server.LogLevel)
	assert.Equal(t, "sqlite", cfg.Database.Driver)
	assert.Equal(t, "/tmp/levee/data/levee.db", cfg.Database.Path)
	assert.Equal(t, 20, cfg.Database.MaxOpenConns)
	assert.Equal(t, 10*time.Minute, cfg.Database.ConnMaxLifetime)
	assert.Equal(t, "debug", cfg.Log.Level) // explicitly overridden
	assert.Equal(t, 10, cfg.Executor.DefaultConcurrency)
	assert.Equal(t, 50, cfg.Executor.MaxConcurrency)
	assert.Equal(t, 5*time.Second, cfg.Executor.ConnectTimeout)
	assert.Equal(t, 2, cfg.Executor.RateLimit.PerTarget)
	assert.Equal(t, 22, cfg.Channel.SSH.Port)
	assert.Equal(t, "key", cfg.Channel.SSH.AuthMethod)
	assert.Equal(t, 5985, cfg.Channel.WinRM.Port)
	assert.Equal(t, "negotiate", cfg.Channel.WinRM.Transport)
	assert.Equal(t, 2*time.Hour, cfg.Approval.StandardTimeout)
	assert.Equal(t, 30, cfg.Audit.RetentionDays)
	assert.Equal(t, "json", cfg.Audit.ExportFormat)
	assert.True(t, cfg.Audit.HashChain)
	assert.False(t, cfg.Audit.WormStorage)
	assert.Equal(t, 30*time.Minute, cfg.Lock.TTL)
	assert.Equal(t, "local", cfg.Credential.Storage)
	assert.False(t, cfg.Notify.Webhook.Enabled)
	assert.Equal(t, "ops", cfg.Permission.DefaultTeam)
	assert.Equal(t, "staging", cfg.Permission.DefaultEnv)
}

func TestLoad_DefaultsOnly(t *testing.T) {
	// No file, no env — every field should fall back to defaults and
	// still pass validation.
	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "info", cfg.Server.LogLevel)
	assert.Equal(t, "text", cfg.Server.LogFormat)
	assert.Equal(t, "sqlite", cfg.Database.Driver)
	assert.NotEmpty(t, cfg.Database.Path) // derived from data_dir
	assert.Equal(t, 50, cfg.Database.MaxOpenConns)
	assert.Equal(t, 10, cfg.Database.MaxIdleConns)
	assert.Equal(t, 30*time.Minute, cfg.Database.ConnMaxLifetime)
	assert.Equal(t, 50, cfg.Executor.DefaultConcurrency)
	assert.Equal(t, 200, cfg.Executor.MaxConcurrency)
	assert.Equal(t, 10*time.Second, cfg.Executor.ConnectTimeout)
	assert.Equal(t, 300*time.Second, cfg.Executor.ExecTimeout)
	assert.Equal(t, 5, cfg.Executor.RateLimit.PerTarget)
	assert.Equal(t, 50, cfg.Executor.RateLimit.PerChannel)
	assert.Equal(t, 100, cfg.Executor.RateLimit.Global)
	assert.Equal(t, 22, cfg.Channel.SSH.Port)
	assert.Equal(t, "key", cfg.Channel.SSH.AuthMethod)
	assert.True(t, cfg.Channel.SSH.StrictHostCheck)
	assert.Equal(t, 10, cfg.Channel.SSH.PoolSize)
	assert.Equal(t, 5985, cfg.Channel.WinRM.Port)
	assert.Equal(t, "negotiate", cfg.Channel.WinRM.Transport)
	assert.Equal(t, 5, cfg.Channel.WinRM.PoolSize)
	assert.Equal(t, 4*time.Hour, cfg.Approval.StandardTimeout)
	assert.Equal(t, 8*time.Hour, cfg.Approval.HighTimeout)
	assert.Equal(t, 24*time.Hour, cfg.Approval.EmergencySupplement)
	assert.Equal(t, 90, cfg.Audit.RetentionDays)
	assert.Equal(t, "json", cfg.Audit.ExportFormat)
	assert.True(t, cfg.Audit.HashChain)
	assert.True(t, cfg.Audit.WormStorage)
	assert.Equal(t, time.Hour, cfg.Lock.TTL)
	assert.Equal(t, 5*time.Second, cfg.Lock.RetryInterval)
	assert.Equal(t, 10, cfg.Lock.MaxRetry)
	assert.Equal(t, "local", cfg.Credential.Storage)
	assert.Equal(t, "aes256-gcm", cfg.Credential.Encryption)
	assert.Equal(t, "argon2id", cfg.Credential.KeyDerivation)
	assert.False(t, cfg.Notify.Webhook.Enabled)
	assert.Equal(t, 3, cfg.Notify.Webhook.Retry)
	assert.Equal(t, "default", cfg.Permission.DefaultTeam)
	assert.Equal(t, "dev", cfg.Permission.DefaultEnv)
}

func TestLoad_EnvOverride(t *testing.T) {
	p := writeYAML(t, minimalValidYAML())

	withEnv(t, map[string]string{
		"LEVEE_SERVER_LOG_LEVEL":         "error",
		"LEVEE_DATABASE_PATH":            "/tmp/levee/override.db",
		"LEVEE_EXECUTOR_MAX_CONCURRENCY": "777",
		"LEVEE_CHANNEL_SSH_PORT":         "2222",
		"LEVEE_AUDIT_RETENTION_DAYS":     "7",
		"LEVEE_PERMISSION_DEFAULT_ENV":   "prod",
	})

	cfg, err := Load(p)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "error", cfg.Server.LogLevel)
	assert.Equal(t, "/tmp/levee/override.db", cfg.Database.Path)
	assert.Equal(t, 777, cfg.Executor.MaxConcurrency)
	assert.Equal(t, 2222, cfg.Channel.SSH.Port)
	assert.Equal(t, 7, cfg.Audit.RetentionDays)
	assert.Equal(t, "prod", cfg.Permission.DefaultEnv)

	// Non-overridden fields keep their file values.
	assert.Equal(t, "/tmp/levee/data", cfg.Server.DataDir)
	assert.Equal(t, 10, cfg.Executor.DefaultConcurrency)
}

func TestLoad_EnvOverrideOnly(t *testing.T) {
	// No file — env + defaults only. Validates that AutomaticEnv works
	// without a config file present.
	withEnv(t, map[string]string{
		"LEVEE_SERVER_DATA_DIR":              "/tmp/levee/envonly",
		"LEVEE_SERVER_LOG_LEVEL":             "warn",
		"LEVEE_DATABASE_PATH":                "/tmp/levee/envonly.db",
		"LEVEE_EXECUTOR_DEFAULT_CONCURRENCY": "9",
	})

	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "/tmp/levee/envonly", cfg.Server.DataDir)
	assert.Equal(t, "warn", cfg.Server.LogLevel)
	assert.Equal(t, "/tmp/levee/envonly.db", cfg.Database.Path)
	assert.Equal(t, 9, cfg.Executor.DefaultConcurrency)
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "config file not found")
}

func TestLoad_UnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(p, []byte("x = 1"), 0o644))

	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported config extension")
}

func TestLoad_DirectoryPath(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "directory")
}

func TestLoad_MalformedYAML(t *testing.T) {
	p := writeYAML(t, "server: [this is not valid yaml\n  - broken")
	_, err := Load(p)
	require.Error(t, err)
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Server.LogLevel = "trace"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.log_level")
}

func TestValidate_InvalidLogFormat(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Server.LogFormat = "xml"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.log_format")
}

func TestValidate_InvalidDBDriver(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Database.Driver = "postgres"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "database.driver")
}

func TestValidate_IdleExceedsOpen(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Database.MaxOpenConns = 5
	cfg.Database.MaxIdleConns = 10
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_idle_conns")
}

func TestValidate_ConcurrencyRange(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Executor.DefaultConcurrency = 100
	cfg.Executor.MaxConcurrency = 50
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "default_concurrency")
}

func TestValidate_InvalidSSHPort(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Channel.SSH.Port = 70000
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ssh.port")
}

func TestValidate_SSHKeyRequiredForKeyAuth(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Channel.SSH.AuthMethod = "key"
	cfg.Channel.SSH.KeyPath = ""
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key_path")
}

func TestValidate_SSHPasswordAuthNoKeyNeeded(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Channel.SSH.AuthMethod = "password"
	cfg.Channel.SSH.KeyPath = ""
	err = Validate(cfg)
	require.NoError(t, err)
}

func TestValidate_InvalidWinRMTransport(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Channel.WinRM.Transport = "ntlm"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "winrm.transport")
}

func TestValidate_WebhookEnabledRequiresURL(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Notify.Webhook.Enabled = true
	cfg.Notify.Webhook.URL = ""
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webhook.url")
}

func TestValidate_WebhookDisabledNoURLNeeded(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Notify.Webhook.Enabled = false
	cfg.Notify.Webhook.URL = ""
	err = Validate(cfg)
	require.NoError(t, err)
}

func TestValidate_InvalidExportFormat(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Audit.ExportFormat = "xml"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "export_format")
}

func TestValidate_InvalidCredentialStorage(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Credential.Storage = "s3"
	err = Validate(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credential.storage")
}

func TestValidate_MultipleProblemsReported(t *testing.T) {
	cfg, err := Load(writeYAML(t, minimalValidYAML()))
	require.NoError(t, err)

	cfg.Server.LogLevel = "trace"
	cfg.Database.Driver = "postgres"
	cfg.Channel.SSH.Port = 0
	cfg.Permission.DefaultTeam = ""

	err = Validate(cfg)
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "server.log_level")
	assert.Contains(t, msg, "database.driver")
	assert.Contains(t, msg, "ssh.port")
	assert.Contains(t, msg, "default_team")
}

func TestLoad_HomeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	p := writeYAML(t, `server:
  data_dir: ~/levee-test-data
  log_level: info
  log_format: text
channel:
  ssh:
    auth_method: password
`)
	cfg, err := Load(p)
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "levee-test-data"), cfg.Server.DataDir)
	// Database path derived from expanded data_dir.
	assert.Contains(t, cfg.Database.Path, "levee-test-data")
}

func TestLoad_LogMirrorsServerWhenAbsent(t *testing.T) {
	// config.yaml has no top-level `log:` section; log.level/format
	// should mirror server.log_level/log_format.
	p := writeYAML(t, `server:
  data_dir: /tmp/levee/data
  log_level: warn
  log_format: json
channel:
  ssh:
    auth_method: password
`)
	cfg, err := Load(p)
	require.NoError(t, err)

	assert.Equal(t, "warn", cfg.Log.Level)
	assert.Equal(t, "json", cfg.Log.Format)
}
