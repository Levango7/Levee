// HashiCorp Vault provider for the credential package.
//
// Implements KMSProvider using the Vault KV v2 secrets engine. Authentication
// is via AppRole (role_id + secret_id). Leases are tracked so ReturnSecret
// can revoke them, and a background renewer keeps long-lived leases alive
// until the caller signals it is done with the secret.
//
// Security:
//   - Plaintext is returned to the caller and never retained by the provider.
//   - The role_id / secret_id are kept in memory only; they are never logged.
//   - Lease IDs are opaque Vault strings; logging them is safe and useful
//     for debugging, but we still redact them in production logs via the
//     redactLease helper.

package credential

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/vault/api"

	"github.com/nexus/levee/internal/log"
)

// --- Vault sentinel errors --------------------------------------------------

var (
	// ErrVaultNotInitialised is returned when the provider has not been
	// initialised (e.g. AppRole login failed at construction time and the
	// caller chose to defer login).
	ErrVaultNotInitialised = errors.New("credential: vault provider not initialised")

	// ErrVaultSecretNotFound is returned when the requested secret does
	// not exist in the KV v2 path.
	ErrVaultSecretNotFound = errors.New("credential: vault secret not found")

	// ErrVaultMissingRoleID is returned when the AppRole role_id is empty.
	ErrVaultMissingRoleID = errors.New("credential: vault role_id is required")

	// ErrVaultMissingSecretID is returned when the AppRole secret_id is empty.
	ErrVaultMissingSecretID = errors.New("credential: vault secret_id is required")

	// ErrVaultMissingAddress is returned when the Vault address is empty.
	ErrVaultMissingAddress = errors.New("credential: vault address is required")
)

// --- VaultProviderConfig ---------------------------------------------------

// VaultProviderConfig configures the Vault provider.
type VaultProviderConfig struct {
	// Address is the Vault server URL (e.g. https://vault.example.com:8200).
	Address string

	// RoleID and SecretID are the AppRole credentials.
	RoleID   string
	SecretID string

	// MountPath is the KV v2 mount path (default "secret").
	MountPath string

	// Namespace is the Vault Enterprise namespace (optional; empty for OSS).
	Namespace string

	// Timeout is the per-request timeout for Vault API calls. Defaults to
	// 10s when zero.
	Timeout time.Duration

	// MaxRetries is the number of times to retry transient failures.
	// Defaults to 3 when zero.
	MaxRetries int

	// RenewInterval is how often to renew leases in the background. A
	// value of 0 disables auto-renew (leases will expire at their TTL).
	RenewInterval time.Duration

	// TLSConfig holds TLS settings. When nil, system roots are used.
	TLSConfig *VaultTLSConfig
}

// VaultTLSConfig holds TLS settings for the Vault client.
type VaultTLSConfig struct {
	CACert     string // PEM-encoded CA cert path
	ClientCert string // client cert path
	ClientKey  string // client key path
	Insecure   bool   // skip TLS verification (NOT for production)
}

// --- VaultProvider ---------------------------------------------------------

// VaultProvider implements KMSProvider against a HashiCorp Vault server
// using the KV v2 secrets engine and AppRole authentication.
//
// The provider tracks active leases in a sync.Map so ReturnSecret can
// revoke them. A background goroutine optionally renews leases before they
// expire; it is started by StartRenewer and stopped by Close.
type VaultProvider struct {
	cfg    VaultProviderConfig
	client *api.Client

	// leases maps leaseID → *api.Secret (the renewal descriptor). It is
	// safe for concurrent access.
	leases sync.Map

	// renewerCancel stops the background renewer goroutine when set.
	renewerCancel context.CancelFunc
	renewerOnce   sync.Once

	// closed protects against double-close.
	closed bool
	mu     sync.Mutex
}

// NewVaultProvider constructs a VaultProvider and performs the initial
// AppRole login. The returned provider is ready to use; call StartRenewer
// (optional) to enable background lease renewal.
//
// Required config fields: Address, RoleID, SecretID. MountPath defaults to
// "secret" when empty. Timeout defaults to 10s when zero. MaxRetries
// defaults to 3 when zero.
func NewVaultProvider(ctx context.Context, cfg VaultProviderConfig) (*VaultProvider, error) {
	if cfg.Address == "" {
		return nil, ErrVaultMissingAddress
	}
	if cfg.RoleID == "" {
		return nil, ErrVaultMissingRoleID
	}
	if cfg.SecretID == "" {
		return nil, ErrVaultMissingSecretID
	}
	if cfg.MountPath == "" {
		cfg.MountPath = "secret"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}

	vcfg := api.DefaultConfig()
	vcfg.Address = cfg.Address
	if cfg.Timeout > 0 {
		vcfg.Timeout = cfg.Timeout
	}
	if cfg.TLSConfig != nil {
		if err := applyVaultTLS(vcfg, cfg.TLSConfig); err != nil {
			return nil, fmt.Errorf("credential: vault tls: %w", err)
		}
	}

	client, err := api.NewClient(vcfg)
	if err != nil {
		return nil, fmt.Errorf("credential: vault client: %w", err)
	}
	if cfg.Namespace != "" {
		client.SetNamespace(cfg.Namespace)
	}

	p := &VaultProvider{
		cfg:    cfg,
		client: client,
	}

	// AppRole login.
	if err := p.login(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// applyVaultTLS wires the TLS config into the Vault client config. The
// Vault api.Config does not expose CACert/ClientCert/ClientKey fields
// directly (they live on an internal helper); we instead build an
// *http.Transport with the appropriate tls.Config and attach it to the
// HttpClient. When Insecure is set we skip verification entirely.
func applyVaultTLS(vcfg *api.Config, t *VaultTLSConfig) error {
	// Start from the existing transport if present, otherwise a fresh one.
	transport, ok := vcfg.HttpClient.Transport.(*http.Transport)
	if !ok || transport == nil {
		transport = &http.Transport{}
	}
	tlsCfg := transport.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if t.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	if t.CACert != "" {
		pool, err := loadCACertPool(t.CACert)
		if err != nil {
			return fmt.Errorf("load ca cert: %w", err)
		}
		tlsCfg.RootCAs = pool
	}
	if t.ClientCert != "" && t.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(t.ClientCert, t.ClientKey)
		if err != nil {
			return fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	transport.TLSClientConfig = tlsCfg
	vcfg.HttpClient.Transport = transport
	return nil
}

// login performs AppRole authentication and stores the resulting token on
// the client. The login token's lease is NOT tracked here; Vault client
// tokens are managed separately from secret leases.
func (p *VaultProvider) login(ctx context.Context) error {
	secret, err := p.client.Logical().WriteWithContext(ctx, "auth/approle/login", map[string]any{
		"role_id":   p.cfg.RoleID,
		"secret_id": p.cfg.SecretID,
	})
	if err != nil {
		return fmt.Errorf("credential: vault approle login: %w", err)
	}
	if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
		return fmt.Errorf("credential: vault approle login: no client token in response")
	}
	p.client.SetToken(secret.Auth.ClientToken)
	log.DebugCtx(ctx, "vault approle login succeeded")
	return nil
}

// Name returns the provider identifier.
func (p *VaultProvider) Name() string { return "vault" }

// --- KMSProvider: GetSecret ------------------------------------------------

// GetSecret reads the latest version of a KV v2 secret at path
// <mount>/data/<name> and returns the value of the "value" field as the
// plaintext. The lease ID returned is the Vault lease ID of the read
// response (when present); pass it to ReturnSecret to revoke.
//
// The secret name is interpreted as a path relative to the mount, e.g.
// "prod/db/password" reads from "secret/data/prod/db/password". The field
// name within the secret data is "value" by convention; if absent, the
// provider returns the JSON-marshalled secret data as a fallback so
// callers can store arbitrary structured secrets.
func (p *VaultProvider) GetSecret(ctx context.Context, name string) ([]byte, string, error) {
	if name == "" {
		return nil, "", ErrEmptyName
	}

	// KV v2 read path: <mount>/data/<name>.
	path := fmt.Sprintf("%s/data/%s", p.cfg.MountPath, name)
	secret, err := p.client.Logical().ReadWithDataWithContext(ctx, path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("credential: vault read %q: %w", path, err)
	}
	if secret == nil {
		return nil, "", fmt.Errorf("credential: vault secret %q: %w", name, ErrVaultSecretNotFound)
	}

	// KV v2 wraps the data under a "data" sub-key.
	data, ok := secret.Data["data"].(map[string]any)
	if !ok || data == nil {
		return nil, "", fmt.Errorf("credential: vault secret %q: malformed kv-v2 payload", name)
	}

	plaintext, err := extractVaultValue(data)
	if err != nil {
		return nil, "", fmt.Errorf("credential: vault secret %q: %w", name, err)
	}

	leaseID := secret.LeaseID
	if leaseID != "" {
		p.leases.Store(leaseID, secret)
	}

	return plaintext, leaseID, nil
}

// extractVaultValue pulls the "value" field from the KV v2 data map. When
// "value" is absent, it falls back to JSON-marshalling the whole data map
// so structured secrets still work.
func extractVaultValue(data map[string]any) ([]byte, error) {
	if v, ok := data["value"]; ok {
		switch t := v.(type) {
		case string:
			return []byte(t), nil
		case []byte:
			return t, nil
		default:
			// Fall through to JSON fallback for non-string values.
		}
	}
	// Fallback: marshal the whole data map. We import encoding/json inline
	// via a small helper to avoid a top-level import that would be unused
	// when "value" is always present.
	return vaultJSONFallback(data)
}

// vaultJSONFallback marshals the data map to JSON. It is a separate
// function so the encoding/json import stays localised.
func vaultJSONFallback(data map[string]any) ([]byte, error) {
	return jsonMarshal(data)
}

// --- KMSProvider: ReturnSecret ---------------------------------------------

// ReturnSecret revokes the lease identified by leaseID. It is safe to call
// with an empty leaseID (no-op) and safe to call multiple times. Errors
// from Vault are returned but the lease is removed from the tracking map
// regardless.
func (p *VaultProvider) ReturnSecret(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return nil
	}
	p.leases.Delete(leaseID)

	err := p.client.Sys().RevokeWithContext(ctx, leaseID)
	if err != nil {
		// Treat already-revoked leases as success.
		if strings.Contains(err.Error(), "invalid lease") || strings.Contains(err.Error(), "lease not found") {
			return nil
		}
		return fmt.Errorf("credential: vault revoke lease %q: %w", redactLease(leaseID), err)
	}
	return nil
}

// --- KMSProvider: HealthCheck ----------------------------------------------

// HealthCheck verifies that the Vault server is reachable and the client
// token is still valid. It calls sys/health and returns nil when the
// server is sealed-unsealed and the token is live.
func (p *VaultProvider) HealthCheck(ctx context.Context) error {
	health, err := p.client.Sys().HealthWithContext(ctx)
	if err != nil {
		return fmt.Errorf("credential: vault health: %w", err)
	}
	if health == nil {
		return fmt.Errorf("credential: vault health: nil response")
	}
	if !health.Initialized {
		return fmt.Errorf("credential: vault not initialised")
	}
	if health.Sealed {
		return fmt.Errorf("credential: vault sealed")
	}
	// Verify the token is still valid by doing a cheap lookup-self.
	_, err = p.client.Auth().Token().LookupSelfWithContext(ctx)
	if err != nil {
		return fmt.Errorf("credential: vault token lookup: %w", err)
	}
	return nil
}

// --- Background lease renewal ----------------------------------------------

// StartRenewer starts a background goroutine that renews active leases
// every RenewInterval. It is safe to call multiple times; only the first
// call starts the renewer. Call Close to stop it.
func (p *VaultProvider) StartRenewer() {
	if p.cfg.RenewInterval <= 0 {
		return
	}
	p.renewerOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		p.renewerCancel = cancel
		go p.renewerLoop(ctx)
	})
}

// renewerLoop periodically iterates over tracked leases and renews them.
func (p *VaultProvider) renewerLoop(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.renewAll(ctx)
		}
	}
}

// renewAll attempts to renew every tracked lease. Renewal failures are
// logged but do not stop the loop.
func (p *VaultProvider) renewAll(ctx context.Context) {
	p.leases.Range(func(key, value any) bool {
		leaseID, _ := key.(string)
		secret, _ := value.(*api.Secret)
		if secret == nil {
			p.leases.Delete(leaseID)
			return true
		}
		// Sys().RenewWithContext renews by lease ID; we pass the original
		// increment (0 = use default TTL) so Vault refreshes to full TTL.
		renewed, err := p.client.Sys().RenewWithContext(ctx, leaseID, 0)
		if err != nil {
			log.WarnCtx(ctx, "vault lease renew failed",
				"lease", redactLease(leaseID), "err", err.Error())
			p.leases.Delete(leaseID)
			return true
		}
		if renewed != nil {
			p.leases.Store(leaseID, renewed)
		}
		return true
	})
}

// --- Close -----------------------------------------------------------------

// Close stops the background renewer (if started) and revokes all tracked
// leases. It is safe to call multiple times. The Vault client token is
// revoked as well, rendering the provider unusable afterwards.
func (p *VaultProvider) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true

	if p.renewerCancel != nil {
		p.renewerCancel()
	}

	// Revoke all tracked leases.
	var revokeErrs []error
	p.leases.Range(func(key, value any) bool {
		leaseID, _ := key.(string)
		if err := p.ReturnSecret(ctx, leaseID); err != nil {
			revokeErrs = append(revokeErrs, err)
		}
		return true
	})

	// Revoke the client token.
	if token := p.client.Token(); token != "" {
		if err := p.client.Auth().Token().RevokeSelfWithContext(ctx, token); err != nil {
			revokeErrs = append(revokeErrs, fmt.Errorf("revoke self token: %w", err))
		}
	}

	if len(revokeErrs) > 0 {
		return fmt.Errorf("credential: vault close: %v", revokeErrs[0])
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

// redactLease returns a redacted form of the lease ID suitable for logging.
// It keeps the first 8 characters (or the whole ID when shorter) and
// replaces the rest with "***".
func redactLease(leaseID string) string {
	if len(leaseID) <= 8 {
		return leaseID
	}
	return leaseID[:8] + "***"
}

// jsonMarshal is a small wrapper around encoding/json.Marshal so the
// import stays localised to the fallback path.
func jsonMarshal(v any) ([]byte, error) {
	return jsonMarshalImpl(v)
}
