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

	"github.com/nexus/levee/internal/state"
)

// Config is the root configuration object for LEVEE. It mirrors the
// structure of configs/config.yaml and is the single source of truth
// for all subsystem configuration.
type Config struct {
	Server     ServerConfig     `json:"server"     mapstructure:"server"`
	Auth       AuthConfig       `json:"auth"       mapstructure:"auth"`
	AI         AIConfig         `json:"ai"         mapstructure:"ai"`
	Tenant     TenantConfig     `json:"tenant"     mapstructure:"tenant"`
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
	Tracing    TracingConfig    `json:"tracing"    mapstructure:"tracing"`
	Diagnosis  DiagnosisConfig  `json:"diagnosis"  mapstructure:"diagnosis"`
	Drift      DriftConfig      `json:"drift"      mapstructure:"drift"`
	OpsMesh    OpsMeshConfig    `json:"opsmesh"    mapstructure:"opsmesh"`
	Security   SecurityConfig   `json:"security"   mapstructure:"security"`
	State      StateConfig      `json:"state"      mapstructure:"state"`
}

// DriftConfig groups drift-detection settings. The schedule subsection is
// the serve face of the drift scheduler: `levee drift schedule add` (CLI)
// writes job files under <dataDir>/drift/jobs and this switch decides
// whether a serve process runs them in the background — one job store, two
// consumers, no duplicated configuration.
type DriftConfig struct {
	Schedule DriftScheduleConfig `json:"schedule" mapstructure:"schedule"`
}

// DriftScheduleConfig enables the resident drift scheduler inside serve.
// Enabled defaults to false: an existing deployment keeps behaving exactly
// as before (drift checks only when someone runs the CLI commands), and
// turning it on is a deliberate act. Jobs themselves are never configured
// here — they live as files managed by `levee drift schedule add`, shared
// with the CLI, so CLI and serve cannot disagree about what is scheduled.
type DriftScheduleConfig struct {
	Enabled bool `json:"enabled" mapstructure:"enabled"`
}

// DiagnosisConfig groups the optional diagnosis-pipeline extensions.
type DiagnosisConfig struct {
	Topology TopologySourceConfig `json:"topology" mapstructure:"topology"`
}

// TopologySourceConfig configures the APM topology source behind the
// diagnosis impact-radius stage (flow step 3d of the OpsMesh integration
// design). Provider empty (the default) disables the stage — a deployment
// without an APM backend behaves exactly as before. A configured provider
// without an endpoint, or an unknown provider, degrades with a warning
// rather than keeping the daemon from starting (the setupServeTracing
// contract: an optional evidence source must not block diagnosis).
type TopologySourceConfig struct {
	// Provider selects the topology backend: "skywalking" | "pinpoint" |
	// "opsmesh". The opsmesh provider needs opsmesh.enabled + base_url and
	// pulls the platform's catalog graph through /api/v1/catalog/topology.
	Provider string `json:"provider" mapstructure:"provider"`
	// Endpoint is the APM API base URL (e.g. http://skywalking:12800).
	// Ignored by the opsmesh provider, which uses opsmesh.base_url.
	Endpoint string `json:"endpoint" mapstructure:"endpoint"`
	// TenantID is the opsmesh provider's tenant scope. Empty lets the
	// platform apply its default tenant. Ignored by APM providers.
	TenantID string `json:"tenant_id" mapstructure:"tenant_id"`
	// Timeout is the per-collect HTTP timeout. Zero defaults to 10s.
	Timeout time.Duration `json:"timeout" mapstructure:"timeout"`
}

// OpsMeshConfig configures the OpsMesh platform integration client
// (docs/opsmesh-integration-design.md §6.2): the outcome of alert-driven
// fixes is reported back to the platform so it can close its alerts and
// train its recommendation engine. Disabled (the default) installs nothing.
// Enabled requires base_url, else the boot fails loudly.
//
// Boundary: only the RESULT-REPORT direction is wired today. The topology /
// metrics PULL methods need OpsMesh-side addressing semantics (the API takes
// a service name; diagnoses carry host targets) and are documented as
// pending that cross-system decision rather than guessed at here.
type OpsMeshConfig struct {
	Enabled bool          `json:"enabled" mapstructure:"enabled"`
	BaseURL string        `json:"base_url" mapstructure:"base_url"`
	APIKey  string        `json:"api_key"  mapstructure:"api_key"`
	Timeout time.Duration `json:"timeout"  mapstructure:"timeout"`
}

// ServerConfig holds server-mode runtime parameters.
type ServerConfig struct {
	DataDir   string `json:"data_dir"   mapstructure:"data_dir"`
	LogLevel  string `json:"log_level"  mapstructure:"log_level"`
	LogFormat string `json:"log_format" mapstructure:"log_format"`
}

// DatabaseConfig names the one database a process works against. `driver`
// selects the backend: state.DriverSQLite (the default, a single embedded file
// under Server.DataDir) or state.DriverPostgres, which additionally requires
// `dsn`.
//
// The pair is deliberately the *only* store selector: every entry point that
// holds a configuration (CLI commands, `serve` without --cluster, `system
// status`/`doctor`, the change calendar) opens through the same decision, so
// "which database did this write land in" has one answer per config file. See
// openStoreFromConfig in cmd/levee/helpers.go.
type DatabaseConfig struct {
	Driver string `json:"driver" mapstructure:"driver"` // sqlite | postgres
	Path   string `json:"path"   mapstructure:"path"`   // db file path (sqlite only)
	// DSN is the PostgreSQL connection string (pgx format, e.g.
	// "postgres://user:pass@host:5432/levee?sslmode=require"). It is required
	// when driver is postgres, and setting it under the sqlite driver is a
	// config error rather than something to ignore: an unread DSN sitting in a
	// deployment file is a credential leak that also quietly means the
	// operator's intent was not carried out.
	DSN string `json:"dsn" mapstructure:"dsn"`
	// Pool tuning
	MaxOpenConns    int           `json:"max_open_conns"    mapstructure:"max_open_conns"`
	MaxIdleConns    int           `json:"max_idle_conns"    mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `json:"conn_max_lifetime" mapstructure:"conn_max_lifetime"`
}

// StoreLocation describes which database this section points at, in a form
// safe to print or log: the SQLite file path, or "postgres@host:port/db" with
// the credentials removed. It never returns the raw DSN.
func (d DatabaseConfig) StoreLocation() string {
	if d.Driver == state.DriverPostgres {
		return state.RedactDSN(d.DSN)
	}
	return d.Path
}

// AuthConfig groups API authentication settings. Static bearer tokens are
// configured via flags/env (--token, --auth-token, LEVEE_TOKEN) rather than
// this file; this section carries the SSO providers.
type AuthConfig struct {
	OIDC   AuthOIDCConfig   `json:"oidc"   mapstructure:"oidc"`
	GitHub AuthGitHubConfig `json:"github" mapstructure:"github"`
}

// AIConfig groups AI-assist settings. The LLM subsection is the config face
// of the recommend engine's hybrid mode; disabled (default) the engine runs
// pure knowledge-base mode exactly as before this section existed.
type AIConfig struct {
	LLM LLMConfig `json:"llm" mapstructure:"llm"`
}

// LLMConfig configures the LLM client behind the recommend engine's hybrid
// mode. Enabled defaults to false — no client is constructed and the engine
// takes its knowledge-base path — so an existing deployment behaves exactly
// as before; enabling is a deliberate act. The client itself comes from
// internal/recommend (provider openai / ollama); construction errors degrade
// to knowledge-base mode with a loud warning rather than keeping the daemon
// from starting (the same contract as setupServeTracing).
type LLMConfig struct {
	Enabled     bool          `json:"enabled"      mapstructure:"enabled"`
	Provider    string        `json:"provider"     mapstructure:"provider"` // openai | ollama ("mock" is tests only)
	APIKey      string        `json:"api_key"      mapstructure:"api_key"`  // falls back to OPENAI_API_KEY env
	Model       string        `json:"model"        mapstructure:"model"`
	BaseURL     string        `json:"base_url"     mapstructure:"base_url"` // empty = provider default
	MaxTokens   int           `json:"max_tokens"   mapstructure:"max_tokens"`
	Temperature float64       `json:"temperature"  mapstructure:"temperature"`
	Timeout     time.Duration `json:"timeout"      mapstructure:"timeout"`
}

// TenantConfig controls multi-tenant isolation.
//
// DEFAULT IS OFF, and that default is load-bearing: with it off the gateway
// performs no tenant resolution at all and every row belongs to
// state.DefaultTenantID, so an existing single-tenant deployment behaves
// exactly as it did before tenant_id existed.
//
// Turning it on is a deliberate, reviewed act. When enabled, a request whose
// credential carries no tenant is REFUSED rather than defaulted — a
// configuration mistake surfaces as a failed login, not as a silent collapse
// of every tenant into one. Startup also refuses to enable isolation unless at
// least one credential is bound to a tenant, so a half-configured deployment
// fails to boot rather than serving an unisolated API.
type TenantConfig struct {
	// Enabled turns on per-request tenant resolution and isolation.
	Enabled bool `json:"enabled" mapstructure:"enabled"`
}

// AuthGitHubConfig configures the GitHub OAuth login flow. GitHub is not an
// OIDC provider (opaque tokens, no browser CORS on the token endpoint), so
// the gateway runs the code exchange server-side and mints a LEVEE session
// token instead; see internal/auth/github.go.
//
// When org is empty ANY GitHub user that completes the OAuth grant can sign
// in — set it for internal deployments. team_role_map maps "org/team" slugs
// to LEVEE roles (file-only; env vars cannot express nested maps).
type AuthGitHubConfig struct {
	Enabled bool `json:"enabled" mapstructure:"enabled"`
	// ClientID / ClientSecret come from a GitHub OAuth App registration
	// (Settings → Developer settings → OAuth Apps).
	ClientID     string `json:"client_id"     mapstructure:"client_id"`
	ClientSecret string `json:"client_secret" mapstructure:"client_secret"`
	// Org restricts login to members of this GitHub org. Empty admits any
	// GitHub user.
	Org string `json:"org" mapstructure:"org"`
	// TeamRoleMap maps "org/team-slug" to LEVEE role names. Teams absent
	// from the map contribute nothing.
	TeamRoleMap map[string]string `json:"team_role_map" mapstructure:"team_role_map"`
	// SessionSecret is the HMAC key for LEVEE session tokens minted by SSO
	// logins. Empty selects a per-process random key: sessions die on
	// restart and do not work across multiple nodes. At least 32 chars.
	SessionSecret string `json:"session_secret" mapstructure:"session_secret"`
}

// AuthOIDCConfig configures OpenID Connect authentication as an alternative
// to static bearer tokens. When enabled, tokens that look like JWTs are
// verified against the issuer; static tokens keep working alongside and are
// checked first. Discovery is fetched at serve startup and a failure is
// fatal (fail-fast), so a misconfigured issuer cannot silently disable auth.
//
// role_map maps IdP role claims to LEVEE role names; unmapped roles pass
// through unchanged. role_map is file-only — environment variables cannot
// express nested maps.
type AuthOIDCConfig struct {
	Enabled bool `json:"enabled" mapstructure:"enabled"`
	// IssuerURL is the OIDC issuer, e.g. https://idp.example.com. Discovery
	// is read from <IssuerURL>/.well-known/openid-configuration.
	IssuerURL string `json:"issuer_url" mapstructure:"issuer_url"`
	// ClientID is the OIDC client identifier LEVEE expects in the aud claim.
	ClientID string `json:"client_id" mapstructure:"client_id"`
	// Audience overrides ClientID as the expected audience when the IdP
	// issues access tokens with a distinct API audience. Empty means use
	// ClientID.
	Audience string `json:"audience" mapstructure:"audience"`
	// UsernameClaim is the ID-token claim used as the audit subject.
	// Empty defaults to preferred_username.
	UsernameClaim string `json:"username_claim" mapstructure:"username_claim"`
	// RoleClaim names the token claim holding the user's roles. Empty
	// disables role extraction (roles pass as empty).
	RoleClaim string `json:"role_claim" mapstructure:"role_claim"`
	// TenantClaim names the token claim holding the tenant the caller acts
	// for. It is read from the VERIFIED token. Required in practice when
	// tenant.enabled is true and OIDC is the identity source: a token with no
	// value for this claim carries no tenant and is refused.
	TenantClaim string `json:"tenant_claim" mapstructure:"tenant_claim"`
	// RoleMap translates claim roles into LEVEE roles.
	RoleMap map[string]string `json:"role_map" mapstructure:"role_map"`
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
	// Port overrides the port carried by the target. Zero means "unset":
	// the target's port (then the channel default) wins, so a non-zero
	// shipped default cannot silently override per-target ports. The
	// channel's documented precedence is config > target > default.
	Port int `json:"port"              mapstructure:"port"`
	// AuthMethod pins the authentication strategy. Empty means "infer from
	// the credential" (key when a key path is present, else password) — the
	// historical behaviour. A non-empty value is honoured verbatim and fails
	// closed when the credential cannot serve it.
	AuthMethod string `json:"auth_method"       mapstructure:"auth_method"` // "" | key | password | key+password | password+key
	// KeyPath is expanded and validated here, but it is not forwarded to the
	// SSH transport: the resolved credential's KeyPath governs authentication
	// (see D-9 in docs/blocked-items.md).
	KeyPath         string `json:"key_path"          mapstructure:"key_path"`
	KnownHosts      string `json:"known_hosts"       mapstructure:"known_hosts"`
	StrictHostCheck bool   `json:"strict_host_check" mapstructure:"strict_host_check"`
	// ConnectTimeout bounds one connect operation: the TCP dial AND the SSH
	// handshake. Zero means the channel default (30s); a negative value
	// disables the timeout (no deadline on the connect at all) — the
	// channel's own semantics, now reachable from the config instead of only
	// from code.
	ConnectTimeout time.Duration `json:"connect_timeout"   mapstructure:"connect_timeout"`
	PoolSize       int           `json:"pool_size"         mapstructure:"pool_size"`
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
	Transport      string        `json:"transport"       mapstructure:"transport"` // negotiate | ntlm
	ConnectTimeout time.Duration `json:"connect_timeout" mapstructure:"connect_timeout"`
	PoolSize       int           `json:"pool_size"       mapstructure:"pool_size"`
}

// ApprovalConfig holds the three-tier approval timeouts.
type ApprovalConfig struct {
	StandardTimeout     time.Duration `json:"standard_timeout"     mapstructure:"standard_timeout"`
	HighTimeout         time.Duration `json:"high_timeout"         mapstructure:"high_timeout"`
	EmergencySupplement time.Duration `json:"emergency_supplement" mapstructure:"emergency_supplement"`
	// ExpirySweepInterval is how often the serve-side sweeper settles
	// pending approvals whose timeout has passed (approval.CheckExpiry).
	// Zero or negative disables the sweep entirely: timeouts then never
	// fire and expired approvals stay pending forever — the pre-sweeper
	// behaviour. The timeouts themselves are enforced from
	// approval.expiry_sweep_interval = 1m by default; the interval only
	// bounds detection latency, so a missed tick costs nothing but delay.
	ExpirySweepInterval time.Duration `json:"expiry_sweep_interval" mapstructure:"expiry_sweep_interval"`
}

// AuditConfig controls the tamper-evident audit subsystem.
type AuditConfig struct {
	HashChain     bool   `json:"hash_chain"     mapstructure:"hash_chain"`
	WormStorage   bool   `json:"worm_storage"   mapstructure:"worm_storage"`
	RetentionDays int    `json:"retention_days" mapstructure:"retention_days"`
	ExportFormat  string `json:"export_format"  mapstructure:"export_format"`
	// DigestMode selects the audit hash family for NEW rows: sha256 (default)
	// or sm3 (GM/T 0004-2012). Historical rows are verified by the algorithm
	// encoded in their own hash, so switching never breaks existing chains.
	DigestMode string `json:"digest_mode" mapstructure:"digest_mode"`
}

// SecurityConfig groups hardening knobs that are registered into process
// wide subsystem registries at startup.
type SecurityConfig struct {
	// SensitiveFields appends custom field names to the audit redaction
	// vocabulary on top of the built-in table (see internal/audit/redact.go).
	// Names are matched case-insensitively, exact or via a '_'/-'-' word
	// boundary suffix. No existence validation is performed: an unknown
	// field name is simply a no-op. Deliberately absent from the LEVEE_*
	// environment binding table (allKeys): BindEnv can only surface a
	// scalar, which cannot represent a list.
	SensitiveFields []string `json:"sensitive_fields" mapstructure:"sensitive_fields"`

	// ExposeRawConfig lets `SystemService.GetConfig` serve the configuration
	// WITHOUT secret redaction, but only to a caller that asked for raw output.
	// Default false means the answer is always redacted, whichever transport it
	// arrived on, so an omitted request field can never disclose credentials —
	// see the note on GetConfig about why the field alone cannot be trusted.
	// Enabling it is a deployment decision (the serving process logs a WARN on
	// every raw response, and /system/status reports the switch).
	ExposeRawConfig bool `json:"expose_raw_config" mapstructure:"expose_raw_config"`
}

// StateConfig tunes the state store (SQLite) beyond the connection/pool
// settings in DatabaseConfig.
type StateConfig struct {
	// SQLiteSynchronous sets the SQLite synchronous pragma: "normal"
	// (default, WAL checkpoint-batched fsync — current behaviour) or
	// "full" (fsync per commit; use when audit durability outranks the
	// ~1-2% write amplification).
	SQLiteSynchronous string `json:"sqlite_synchronous" mapstructure:"sqlite_synchronous"`
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

// NotifyConfig configures outbound notifications and mirrors: the generic
// webhook notifier, the outbound ITSM Jira approval mirror and the ChatOps
// approval mirror (Slack / DingTalk / Feishu webhook bots).
type NotifyConfig struct {
	Webhook WebhookConfig `json:"webhook" mapstructure:"webhook"`
	Jira    JiraConfig    `json:"jira"    mapstructure:"jira"`
	ChatOps ChatOpsConfig `json:"chatops" mapstructure:"chatops"`
}

// ChatOpsConfig configures the approval ChatOps mirror: approval lifecycle
// events (requested / decided) are projected onto Slack / DingTalk / Feishu
// webhook bots. Disabled (the default) installs nothing — no bot, no
// outbound call, no observer. Enabled requires at least one bot with a
// webhook URL; a bot missing one fails the boot loudly naming the bot
// (silently skipping it would look like a working mirror that never
// delivers). The observer slots are fan-out, so enabling this alongside the
// Jira mirror delivers to both. LEVEE's store stays the system of record —
// the mirror is one-way visibility.
type ChatOpsConfig struct {
	Enabled bool               `json:"enabled" mapstructure:"enabled"`
	Bots    []ChatOpsBotConfig `json:"bots"    mapstructure:"bots"`
}

// ChatOpsBotConfig is one webhook bot. Platform selects the adapter
// (slack | dingtalk | feishu); name is the registration key. Secret is the
// per-platform signing secret where the adapter implements one (see
// internal/chatops for which adapters currently sign) — declaring it where
// unsupported is harmless.
type ChatOpsBotConfig struct {
	Platform   string        `json:"platform"    mapstructure:"platform"`
	Name       string        `json:"name"        mapstructure:"name"`
	WebhookURL string        `json:"webhook_url" mapstructure:"webhook_url"`
	Secret     string        `json:"secret"      mapstructure:"secret"`
	Timeout    time.Duration `json:"timeout"     mapstructure:"timeout"`
	// IdentityMap maps this platform's user ids to LEVEE subjects, for the
	// inbound decision path: a chat user's open_id / member id / staffId is
	// what the platform signs, and the audit record must name the LEVEE
	// subject who decided. Leave it empty when the two are already the same
	// string (the map is then a no-op). An unmapped id is refused by the
	// approver check, never silently adopted as a subject.
	IdentityMap map[string]string `json:"identity_map" mapstructure:"identity_map"`
}

// WebhookConfig configures the webhook notifier.
type WebhookConfig struct {
	Enabled bool          `json:"enabled" mapstructure:"enabled"`
	URL     string        `json:"url"     mapstructure:"url"`
	Timeout time.Duration `json:"timeout" mapstructure:"timeout"`
	Retry   int           `json:"retry"   mapstructure:"retry"`
}

// JiraConfig configures the outbound ITSM Jira approval bridge. When
// enabled, every approval kickoff creates a Jira issue and approval
// decisions post comments onto it — the ITSM side of the approval
// trail. Disabled (the default) is a complete no-op: nothing dials
// out, no issue is created, the bridge is not installed.
type JiraConfig struct {
	Enabled bool `json:"enabled" mapstructure:"enabled"`
	// URL is the Jira base URL, e.g. https://jira.example.com (the
	// client hits <URL>/rest/api/3/...).
	URL string `json:"url" mapstructure:"url"`
	// APIToken is the bearer token (Atlassian API token or PAT).
	// Prefer the LEVEE_JIRA_TOKEN env var over committing it to the
	// config file; the env var wins when both are set.
	APIToken string `json:"api_token" mapstructure:"api_token"`
	// Email is the account email the token belongs to (used for the
	// Basic auth header Jira Cloud expects; empty keeps Bearer-only).
	Email string `json:"email" mapstructure:"email"`
	// ProjectKey is the Jira project the approval issues land in, e.g.
	// "OPS".
	ProjectKey string `json:"project_key" mapstructure:"project_key"`
	// IssueType is the issue type name, e.g. "Task" or "Change".
	IssueType string `json:"issue_type" mapstructure:"issue_type"`
	// Timeout bounds each HTTP round-trip. Default 10s.
	Timeout time.Duration `json:"timeout" mapstructure:"timeout"`
}

// PermissionConfig holds default permission namespace values.
type PermissionConfig struct {
	DefaultTeam string `json:"default_team" mapstructure:"default_team"`
	DefaultEnv  string `json:"default_env"  mapstructure:"default_env"`
	// BulkGrants authorises the fleet-wide pause-all / resume-all API
	// actions: actor -> granted permission names ("pause:all",
	// "resume:all"), the same vocabulary `levee pause all` honours. Empty
	// (the default) leaves those actions unauthorised-by-absence-of-policy
	// and `serve` logs that posture at startup, so bulk pause keeps working
	// as an incident mitigation instead of depending on a grant list that
	// was never written.
	BulkGrants map[string][]string `json:"bulk_grants,omitempty" mapstructure:"bulk_grants"`
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

// TracingConfig controls the distributed tracing plumbing for the
// change lifecycle. Tracing is opt-in: every field defaults to the
// disabled state so existing deployments are unaffected.
type TracingConfig struct {
	// Enabled turns trace_id generation/propagation and span export on.
	Enabled bool `json:"enabled" mapstructure:"enabled"`
	// Exporter selects the span destination: stdout, otlp or none.
	// Empty behaves like none.
	Exporter string `json:"exporter" mapstructure:"exporter"`
	// Endpoint is the collector address for the future OTLP exporter;
	// unused by the stdout exporter.
	Endpoint string `json:"endpoint" mapstructure:"endpoint"`
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
// sshAuthMethods lists the SSH auth methods this package accepts. It is the
// single source of truth for the accepted vocabulary: the error message and
// every test that walks the accepted set read it from here, so adding a method
// needs one edit rather than three that can disagree.
//
// WHY it exists: the config used to spell the accepted set inside a switch's
// message string ("must be key|password") while the channel implemented a
// fourth value. Nothing compared the two, so a capability the channel had was
// unreachable through configuration, and no test noticed because nothing
// enumerated what the config accepts.
//
// Exported because the drift guard lives in internal/channel/ssh (the side that
// IMPLEMENTS the vocabulary): internal/channel/ssh/config_vocabulary_test.go
// reads this and fails when a method exists there that is absent here. internal/config
// must not import the channel — the guard has to point the other way.
func SSHAuthMethods() []string {
	return []string{"key", "password", "key+password", "password+key"}
}

// ChatOpsPlatforms lists the platform spellings this package accepts for
// notify.chatops.bots[].platform. It is the single source of truth for the
// accepted vocabulary on the config side: the validation message and every
// test that walks the accepted set read it from here, so adding a platform
// needs one edit rather than two that can disagree.
//
// WHY it exists: the config side used to accept nothing at all (the field was
// passed through unchecked) while the serve wiring accepted exactly
// "slack" / "dingtalk" / "feishu" — and the CLI accepted those plus the
// aliases "lark" / "dingding" / "dd", case-insensitively and whitespace-
// tolerant. One field, two vocabularies: a bot configured as "lark" worked
// from the CLI and refused to boot the server, and the refusal message listed
// a hand-written set of platforms that nothing compared against the
// adapters.
//
// The accepted set is exactly what internal/chatops can parse — see
// chatops.ParsePlatform and the guard in
// internal/chatops/config_vocabulary_test.go, which fails when these two
// lists drift apart. The order matches chatops.PlatformSpellings() so every
// message that lists the vocabulary reads the same way.
//
// Exported for the same reason SSHAuthMethods is: the drift guard lives on
// the side that IMPLEMENTS the vocabulary, and it reads this list from here.
func ChatOpsPlatforms() []string {
	return []string{"feishu", "lark", "dingtalk", "dingding", "dd", "slack"}
}

// WinRMTransports returns the auth transports internal/channel/winrm actually
// implements, in canonical spelling. The config validator and the channel must
// share this one list — the previous revision accepted "kerberos" (which the
// channel rejects at Create time: it implements SPNEGO negotiation, not raw
// Kerberos) while refusing "ntlm" (which the channel implements), so an
// operator who trusted the validator could write a config that booted fine and
// then failed on every connection. The list is generated from the same source
// of truth the channel dispatches on; see the guard in internal/channel/winrm.
func WinRMTransports() []string {
	return []string{"negotiate", "ntlm"}
}

// winRMTransportAccepted reports whether s is one of the accepted transport
// spellings. The comparison matches the channel's own dispatch exactly:
// Create switches on strings.ToLower(cfg.Transport) with no trimming, so the
// config side must be just as strict — accepting " negotiate " here while the
// channel refused it would be the same class of drift this guard exists to
// catch. The empty string is NOT accepted: it is not a spelling, and the Load
// path never sees it anyway (SetDefault fills "negotiate").
func winRMTransportAccepted(s string) bool {
	t := strings.ToLower(s)
	for _, v := range WinRMTransports() {
		if t == v {
			return true
		}
	}
	return false
}

// LogLevels lists the log level spellings this package accepts for
// server.log_level and log.level. It is the single source of truth on the
// config side: the validation message and every test that walks the accepted
// set read it from here.
//
// The list mirrors the implementation's explicit branches in
// internal/log.parseLevel — including the "warning" alias. Before this change
// the config accepted only the four lowercase spellings while the
// implementation also honoured "warning" (and any casing or surrounding
// whitespace): one field, two vocabularies, and the config side was the
// narrower one.
//
// Exported so the two lists can be compared: the implementation exports its
// own table (internal/log.Levels / Formats) and the drift guard compares
// them. The guard lives in internal/config's test package (package
// config_test) rather than in internal/log, because internal/config cannot
// import internal/log — internal/config → internal/state → internal/log is
// an import cycle.
func LogLevels() []string {
	return []string{"debug", "info", "warn", "warning", "error"}
}

// LogFormats lists the log format spellings this package accepts for
// server.log_format and log.format. Same shape as LogLevels: the list is the
// implementation's explicit dispatch (internal/log.newLogger switches on
// "json" and treats everything else as text), and the drift guard fails if
// the two drift apart.
func LogFormats() []string {
	return []string{"text", "json"}
}

// LogLevelAccepted reports whether s is one of the accepted level spellings.
// The comparison matches internal/log.parseLevel exactly: case-insensitive and
// whitespace-tolerant, so a config file written as "DEBUG" or " warn " is
// accepted here exactly as it is honoured there.
//
// The empty string is NOT accepted: it means "unset", and postProcess
// resolves it against server.log_level before validation ever sees it.
func LogLevelAccepted(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, v := range LogLevels() {
		if t == v {
			return true
		}
	}
	return false
}

// LogFormatAccepted reports whether s is one of the accepted format
// spellings, with the same case-insensitive, whitespace-tolerant comparison
// internal/log.newLogger applies.
func LogFormatAccepted(s string) bool {
	t := strings.ToLower(strings.TrimSpace(s))
	for _, v := range LogFormats() {
		if t == v {
			return true
		}
	}
	return false
}

// chatOpsPlatformAccepted reports whether s is one of the accepted platform
// spellings. The comparison matches chatops.ParsePlatform: case-insensitive
// and whitespace-tolerant, so a config file written as "Feishu" or " feishu "
// means the same thing here as it does on the CLI.
func chatOpsPlatformAccepted(s string) bool {
	key := strings.ToLower(strings.TrimSpace(s))
	for _, p := range ChatOpsPlatforms() {
		if p == key {
			return true
		}
	}
	return false
}

func Validate(cfg *Config) error { //nolint:gocyclo // inherently complex: validates 30+ fields with distinct rules per type
	var problems []string

	// Server
	if cfg.Server.DataDir == "" {
		problems = append(problems, "server.data_dir is required")
	}
	if !LogLevelAccepted(cfg.Server.LogLevel) {
		problems = append(problems, fmt.Sprintf("server.log_level %q must be one of %s", cfg.Server.LogLevel, strings.Join(LogLevels(), " | ")))
	}
	if !LogFormatAccepted(cfg.Server.LogFormat) {
		problems = append(problems, fmt.Sprintf("server.log_format %q must be one of %s", cfg.Server.LogFormat, strings.Join(LogFormats(), " | ")))
	}

	// Database. `driver` picks the backend and each backend has exactly one
	// mandatory locator: a file for sqlite, a DSN for postgres. Both rules are
	// stated here because every opener in the program goes through this
	// decision — a driver that Validate accepts but no opener implements would
	// surface as "open store: …" at runtime instead.
	switch cfg.Database.Driver {
	case state.DriverSQLite:
		if cfg.Database.Path == "" {
			problems = append(problems, "database.path is required")
		}
		if cfg.Database.DSN != "" {
			problems = append(problems, "database.dsn is only used when database.driver = "+state.DriverPostgres)
		}
	case state.DriverPostgres:
		if cfg.Database.DSN == "" {
			problems = append(problems, "database.dsn is required when database.driver = "+state.DriverPostgres)
		}
	default:
		problems = append(problems, fmt.Sprintf("database.driver %q must be one of %s|%s",
			cfg.Database.Driver, state.DriverSQLite, state.DriverPostgres))
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

	// State (SA-019). Empty is tolerated and behaves like the default
	// ("normal"); an unrecognised value is a config error rather than a
	// silently ignored pragma.
	switch strings.ToLower(strings.TrimSpace(cfg.State.SQLiteSynchronous)) {
	case "", "normal", "full":
	default:
		problems = append(problems, fmt.Sprintf("state.sqlite_synchronous %q must be one of normal|full", cfg.State.SQLiteSynchronous))
	}

	// Log. The vocabulary is the implementation's (internal/log.parseLevel /
	// newLogger), read from LogLevels / LogFormats so the two cannot drift;
	// the comparison is case-insensitive and whitespace-tolerant, matching
	// what the implementation actually honours.
	if !LogLevelAccepted(cfg.Log.Level) {
		problems = append(problems, fmt.Sprintf("log.level %q must be one of %s", cfg.Log.Level, strings.Join(LogLevels(), " | ")))
	}
	if !LogFormatAccepted(cfg.Log.Format) {
		problems = append(problems, fmt.Sprintf("log.format %q must be one of %s", cfg.Log.Format, strings.Join(LogFormats(), " | ")))
	}
	// log.output names the sink: "stdout", "stderr", or a file path. A path
	// is opened in append mode when the logger is wired, so there is no
	// vocabulary to check here — every non-empty value is a legal path. An
	// empty value means "unset" and falls back to the default sink (stderr),
	// the same way an unset log.level falls back to server.log_level.

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
	// Port 0 is legal and means "unset": the target's own port (then the
	// channel default) wins. The channel's precedence is config override >
	// target > default, so shipping a non-zero default here would silently
	// override every per-target port once wired.
	if cfg.Channel.SSH.Port < 0 || cfg.Channel.SSH.Port > 65535 {
		problems = append(problems, "channel.ssh.port must be 0 (unset) or in 1..65535")
	}
	// The non-empty accepted set is exactly what internal/channel/ssh's
	// buildAuthMethods implements — see sshAuthMethods() below and the guard
	// in internal/channel/ssh that fails if these two drift apart. Empty is
	// the additional inference mode: preserve the historical behaviour of
	// choosing the method from the resolved credential's shape.
	switch cfg.Channel.SSH.AuthMethod {
	case "", "key", "password", "key+password", "password+key":
	default:
		problems = append(problems, fmt.Sprintf(
			"channel.ssh.auth_method %q must be one of %s (empty = infer from credential)", cfg.Channel.SSH.AuthMethod,
			strings.Join(SSHAuthMethods(), " | ")))
	}
	// key_path is required only when the KEY is the sole method. For
	// key+password the key is an attempt, not the method: a credential that
	// carries only a password degrades to password, which is the point of the
	// mode — demanding a key path would make it unusable for exactly the
	// deployment that needs it.
	if cfg.Channel.SSH.AuthMethod == "key" && cfg.Channel.SSH.KeyPath == "" {
		problems = append(problems, "channel.ssh.key_path is required when auth_method=key")
	}
	// ConnectTimeout: 0 = channel default (30s), negative = disable the
	// timeout, positive = the override. All three are the channel's own
	// semantics, so nothing to validate here — the channel is the authority.

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
	// The accepted set is exactly what internal/channel/winrm dispatches on
	// (Config.withDefaults + Create's transport switch) — see WinRMTransports()
	// above and the guard in internal/channel/winrm that fails if these two
	// drift apart. Before this change the config accepted negotiate|kerberos
	// while the channel implements negotiate|ntlm: kerberos booted fine and then
	// failed every connection, and ntlm was refused at validation time although
	// the channel could run it.
	if !winRMTransportAccepted(cfg.Channel.WinRM.Transport) {
		problems = append(problems, fmt.Sprintf(
			"channel.winrm.transport %q must be one of %s", cfg.Channel.WinRM.Transport,
			strings.Join(WinRMTransports(), " | ")))
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
	switch cfg.Audit.DigestMode {
	case "", "sha256", "sm3":
	default:
		problems = append(problems, fmt.Sprintf("audit.digest_mode %q must be sha256|sm3", cfg.Audit.DigestMode))
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
	switch cfg.Credential.Encryption {
	case "":
		problems = append(problems, "credential.encryption is required")
	case "aes256-gcm", "sm4-gcm":
		// sm4-gcm is an opt-in national-crypto (GM/T 0002-2012) mode;
		// it writes algorithm-tagged v2 blobs and remains readable by
		// AES-only deployments (see internal/credential).
	default:
		problems = append(problems, fmt.Sprintf("credential.encryption %q must be aes256-gcm|sm4-gcm", cfg.Credential.Encryption))
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

	// Notify.ChatOps
	//
	// The platform spelling is checked only when the mirror is enabled: a
	// disabled section installs nothing, so refusing to boot over a
	// half-written draft would be a regression rather than a safety gain. The
	// moment it is enabled, a typo has to fail loudly and name the bot —
	// a mirror that silently skipped a bot looks exactly like a working
	// mirror that never delivers.
	if cfg.Notify.ChatOps.Enabled {
		for i, b := range cfg.Notify.ChatOps.Bots {
			if !chatOpsPlatformAccepted(b.Platform) {
				problems = append(problems, fmt.Sprintf(
					"notify.chatops.bots[%d].platform %q must be one of %s", i, b.Platform,
					strings.Join(ChatOpsPlatforms(), " | ")))
			}
		}
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

	// AI.LLM. Only checked when enabled: the section is off by default and its
	// zero values must stay harmless, so a disabled deployment never has to fill
	// this in.
	//
	// "mock" is deliberately NOT accepted here. recommend.NewLLMClient maps it to
	// a client that returns canned completions, so a typo'd provider reaching
	// that path would serve fabricated recommendations while every health check
	// and log line said the LLM was fine. Refusing it at startup is the only
	// place that mistake can still be caught.
	if cfg.AI.LLM.Enabled {
		switch cfg.AI.LLM.Provider {
		case "openai", "ollama":
		default:
			problems = append(problems, fmt.Sprintf("ai.llm.provider %q must be one of openai|ollama", cfg.AI.LLM.Provider))
		}
		if cfg.AI.LLM.Timeout <= 0 {
			problems = append(problems, "ai.llm.timeout must be > 0 when the LLM is enabled")
		}
		if cfg.AI.LLM.Temperature < 0 || cfg.AI.LLM.Temperature > 1 {
			problems = append(problems, "ai.llm.temperature must be between 0 and 1")
		}
		// A missing api_key is NOT checked here on purpose: the client falls
		// back to OPENAI_API_KEY, and some gateways front OpenAI with no key at
		// all. Refusing to boot over it would break those; a construction
		// failure already degrades to knowledge-base mode with a warning.
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
	v.SetDefault("database.driver", state.DriverSQLite)
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
	// port/auth_method/connect_timeout ship as "unset" (0/"") so the
	// historical behaviour — target port, credential inference, channel
	// 30s default — is unchanged for operators who never touched these keys;
	// an explicit value now reaches the transport instead of being validated
	// and dropped (D-9).
	v.SetDefault("channel.ssh.port", 0)
	v.SetDefault("channel.ssh.auth_method", "")
	v.SetDefault("channel.ssh.key_path", "~/.ssh/id_rsa")
	v.SetDefault("channel.ssh.known_hosts", "~/.ssh/known_hosts")
	v.SetDefault("channel.ssh.strict_host_check", true)
	v.SetDefault("channel.ssh.connect_timeout", 0)
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
	v.SetDefault("approval.expiry_sweep_interval", "1m")

	// Audit
	v.SetDefault("audit.hash_chain", true)
	v.SetDefault("audit.worm_storage", true)
	v.SetDefault("audit.retention_days", 90)
	v.SetDefault("audit.export_format", "json")
	v.SetDefault("audit.digest_mode", "sha256")

	// Security (yaml-only; see SecurityConfig.SensitiveFields for why this
	// key is not env-bound)
	v.SetDefault("security.sensitive_fields", []string{})
	// Deliberately false: raw configuration (which carries credentials) must be
	// an opt-in, never an accident of a request field defaulting to zero.
	v.SetDefault("security.expose_raw_config", false)

	// State
	v.SetDefault("state.sqlite_synchronous", "normal")

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

	// Tracing (disabled by default; operators opt in explicitly)
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.exporter", "")
	v.SetDefault("tracing.endpoint", "")

	// Auth.OIDC (disabled by default; static bearer tokens stay primary)
	v.SetDefault("auth.oidc.enabled", false)
	v.SetDefault("auth.oidc.issuer_url", "")
	v.SetDefault("auth.oidc.client_id", "")
	v.SetDefault("auth.oidc.audience", "")
	v.SetDefault("auth.oidc.username_claim", "preferred_username")
	v.SetDefault("auth.oidc.role_claim", "")
	v.SetDefault("auth.oidc.tenant_claim", "")
	// Multi-tenancy is OFF by default. See TenantConfig: the default keeps an
	// existing single-tenant deployment byte-for-byte unaffected.
	v.SetDefault("tenant.enabled", false)
	v.SetDefault("auth.oidc.role_map", map[string]string{})

	// Auth.GitHub (disabled by default)
	v.SetDefault("auth.github.enabled", false)
	v.SetDefault("auth.github.client_id", "")
	v.SetDefault("auth.github.client_secret", "")
	v.SetDefault("auth.github.org", "")
	v.SetDefault("auth.github.team_role_map", map[string]string{})
	v.SetDefault("auth.github.session_secret", "")

	// Notify.Jira (outbound ITSM approval bridge; disabled by default)
	v.SetDefault("notify.jira.enabled", false)
	v.SetDefault("notify.jira.url", "")
	v.SetDefault("notify.jira.api_token", "")
	v.SetDefault("notify.jira.email", "")
	v.SetDefault("notify.jira.project_key", "")
	v.SetDefault("notify.jira.issue_type", "Task")
	v.SetDefault("notify.jira.timeout", 10*time.Second)

	// AI.LLM (AI-assist for the recommend engine; OFF by default, so an
	// existing deployment constructs no client at all).
	//
	// The provider default is load-bearing rather than cosmetic:
	// recommend.NewLLMClient treats an EMPTY provider as "mock" and hands back a
	// canned client. Without this default, an operator who set enabled=true and
	// nothing else would silently get fabricated recommendations from a stub —
	// no error, no warning. Defaulting to openai makes that deployment fail
	// loudly instead.
	v.SetDefault("ai.llm.enabled", false)
	v.SetDefault("drift.schedule.enabled", false)
	v.SetDefault("ai.llm.provider", "openai")
	v.SetDefault("ai.llm.api_key", "")
	v.SetDefault("ai.llm.model", "")
	v.SetDefault("ai.llm.base_url", "")
	// 0 means "the client's own built-in default" (see recommend's
	// DefaultMaxTokens and friends), so these mirror what config.example.yaml
	// documents rather than second-guessing the engine.
	v.SetDefault("ai.llm.max_tokens", 0)
	v.SetDefault("ai.llm.temperature", 0.0)
	v.SetDefault("ai.llm.timeout", 30*time.Second)
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
		// Bound deliberately: the DSN carries credentials, and a key that is
		// unreachable from the environment leaves a container only one way to
		// name its database — writing the DSN into the rendered config file,
		// i.e. a plaintext credential in a ConfigMap. With it bound, the chart
		// puts the driver in the ConfigMap and the DSN in a Secret. Every read
		// surface still redacts it (state.RedactDSN, StoreLocation,
		// `system config get database.dsn`).
		"database.dsn",
		"database.max_open_conns", "database.max_idle_conns",
		"database.conn_max_lifetime",
		"state.sqlite_synchronous",
		// security.sensitive_fields stays out (a list cannot be surfaced by
		// BindEnv); this scalar can and is, so a container can opt in without
		// editing a file.
		"security.expose_raw_config",
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
		"audit.retention_days", "audit.export_format", "audit.digest_mode",
		"lock.ttl", "lock.retry_interval", "lock.max_retry",
		"credential.storage", "credential.encryption", "credential.key_derivation",
		"notify.webhook.enabled", "notify.webhook.url",
		"notify.webhook.timeout", "notify.webhook.retry",
		"notify.jira.enabled", "notify.jira.url", "notify.jira.api_token",
		"notify.jira.email", "notify.jira.project_key",
		"notify.jira.issue_type", "notify.jira.timeout",
		"ai.llm.enabled", "ai.llm.provider", "ai.llm.api_key", "ai.llm.model",
		"ai.llm.base_url", "ai.llm.max_tokens", "ai.llm.temperature",
		"ai.llm.timeout",
		"drift.schedule.enabled",
		"permission.default_team", "permission.default_env",
		"verify.prometheus_url",
		"inventory.patrol_interval_seconds",
		"tracing.enabled", "tracing.exporter", "tracing.endpoint",
		"auth.oidc.enabled", "auth.oidc.issuer_url", "auth.oidc.client_id",
		"auth.oidc.audience", "auth.oidc.username_claim", "auth.oidc.role_claim",
		"auth.oidc.role_map", "auth.oidc.tenant_claim",
		"auth.github.enabled", "auth.github.client_id", "auth.github.client_secret",
		"auth.github.org", "auth.github.session_secret", "auth.github.team_role_map",
		"tenant.enabled",
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

	// Mirror server.log_* into log when the log section was not explicitly
	// overridden — keeps both views consistent for callers.
	//
	// This is the precedence the documentation states: `log.*` is the owner
	// of the log settings, `server.log_*` is the legacy alias, and an unset
	// `log.*` inherits from it. An explicitly set `log.*` always wins.
	if cfg.Log.Level == "" {
		cfg.Log.Level = cfg.Server.LogLevel
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = cfg.Server.LogFormat
	}
	// log.output may name a file path, so it gets the same ~ expansion the
	// other path fields get. The two sink names ("stdout" / "stderr") are
	// unaffected — they never start with ~.
	cfg.Log.Output = expandHome(cfg.Log.Output, home)

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
