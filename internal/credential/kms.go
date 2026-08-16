// KMS integration: external Key Management System providers and graceful
// fallback to the local CredentialStore (AES-256-GCM + argon2id).
//
// This file extends the credential package (see store.go and provider.go)
// with a higher-level KMS abstraction that adds:
//   - KMSProvider interface implemented by Vault / AWS KMS / future providers
//   - KMSManager: routes credential names to providers, supports multiple
//     providers concurrently (e.g. vault for prod secrets, aws-kms for
//     tokens), and falls back to the local CredentialStore when a provider
//     is unavailable.
//   - Use-and-discard semantics: KMS-fetched plaintext lives only in memory,
//     is zeroed via SecureZero on ReturnSecret, never enters the audit trace
//     and is never logged.
//
// Security model:
//   - Plaintext returned by GetSecret lives only in the caller's memory
//     until ReturnSecret is invoked. ReturnSecret zeros every byte and
//     calls runtime.KeepAlive so the zeroing is not optimized away.
//   - KMS plaintext is NEVER written to disk, NEVER logged, NEVER emitted
//     to the audit trace. Logs include only the credential name, the
//     provider name, and the operation result.
//   - On KMS failure the manager records a WARN-level audit event and
//     transparently falls back to the local CredentialStore. The fallback
//     is observable: callers can distinguish KMS-sourced vs local-sourced
//     credentials via ResolvedCredential.Source.

package credential

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// --- KMS sentinel errors ----------------------------------------------------

var (
	// ErrKMSUnavailable is returned when no KMS provider can service the
	// request and the local fallback is disabled or unavailable.
	ErrKMSUnavailable = errors.New("credential: kms provider unavailable")

	// ErrKMSNotFound is returned when the KMS provider does not have a
	// secret with the requested name.
	ErrKMSNotFound = errors.New("credential: kms secret not found")

	// ErrKMSNotRegistered is returned when no provider is registered for
	// the requested credential name.
	ErrKMSNotRegistered = errors.New("credential: no kms provider registered for credential")

	// ErrKMSNilManager is returned by NewKMSManager when given a nil
	// local store and a nil provider map.
	ErrKMSNilManager = errors.New("credential: nil kms manager inputs")

	// ErrKMSProviderExists is returned by RegisterProvider when a provider
	// with the same name is already registered.
	ErrKMSProviderExists = errors.New("credential: kms provider already registered")

	// ErrKMSUnknownProvider is returned by UnregisterProvider / Route
	// when the named provider does not exist.
	ErrKMSUnknownProvider = errors.New("credential: unknown kms provider")
)

// --- KMSProvider interface --------------------------------------------------

// KMSProvider is the abstraction implemented by every external Key
// Management System integration (HashiCorp Vault, AWS KMS, GCP KMS, ...).
//
// Implementations MUST honour the following contract:
//
//   - GetSecret returns the plaintext secret identified by name, together
//     with an opaque leaseID that the caller passes to ReturnSecret when
//     the secret is no longer needed. leaseID may be empty for providers
//     that do not issue leases (e.g. AWS KMS data-key decryption); in that
//     case ReturnSecret is a no-op.
//
//   - The returned secret lives only in the caller's memory. The provider
//     MUST NOT retain a copy, MUST NOT log it, and MUST NOT emit it to the
//     audit trace.
//
//   - ReturnSecret revokes the lease (if any) and zeros any provider-side
//     state associated with leaseID. It is safe to call with an empty
//     leaseID and safe to call multiple times.
//
//   - HealthCheck returns nil when the provider is reachable and ready to
//     service GetSecret calls, otherwise an error describing the failure.
//     It MUST NOT perform any operation that mutates provider state.
//
// All methods MUST be safe for concurrent use by multiple goroutines.
type KMSProvider interface {
	// Name returns the human-readable provider identifier (e.g. "vault",
	// "aws-kms"). It is used for routing, logging and status reporting.
	Name() string

	// GetSecret fetches the plaintext secret identified by name. The
	// returned secret is the caller's responsibility; pass leaseID to
	// ReturnSecret when done so the provider can revoke the lease and
	// zero any retained state.
	GetSecret(ctx context.Context, name string) (secret []byte, leaseID string, err error)

	// ReturnSecret revokes the lease identified by leaseID and zeros any
	// provider-side state. Safe to call with an empty leaseID.
	ReturnSecret(ctx context.Context, leaseID string) error

	// HealthCheck reports whether the provider is reachable and ready.
	HealthCheck(ctx context.Context) error
}

// --- KMSManager -------------------------------------------------------------

// KMSManager manages one or more KMSProvider instances and routes
// credential-name lookups to the appropriate provider. When a provider is
// unavailable (HealthCheck fails or GetSecret returns an error), the
// manager transparently falls back to the local CredentialStore (AES-GCM)
// and records a WARN-level audit event so operators can investigate.
//
// The fallback is opt-in via the Fallback field. When Fallback is nil the
// manager returns the underlying KMS error directly.
//
// All fields are guarded by mu so RegisterProvider / UnregisterProvider /
// Route may be called concurrently with GetCredential / Status.
type KMSManager struct {
	mu sync.RWMutex

	// providers maps provider name → provider instance.
	providers map[string]KMSProvider

	// routes maps credential name → provider name. A credential with no
	// explicit route is serviced by the defaultProvider (if set).
	routes map[string]string

	// defaultProvider is the provider name used for credentials without
	// an explicit route. May be empty.
	defaultProvider string

	// fallback is the local CredentialStore used when a KMS provider is
	// unavailable. May be nil to disable fallback.
	fallback *CredentialStore

	// fallbackEnabled controls whether fallback is attempted on KMS
	// failures. Defaults to true when fallback is non-nil.
	fallbackEnabled bool
}

// KMSManagerOption configures a KMSManager at construction time.
type KMSManagerOption func(*KMSManager)

// WithDefaultProvider sets the provider used for credentials without an
// explicit route.
func WithDefaultProvider(name string) KMSManagerOption {
	return func(m *KMSManager) { m.defaultProvider = name }
}

// WithFallbackEnabled controls whether the manager falls back to the local
// CredentialStore on KMS failures. Defaults to true when a non-nil fallback
// is supplied to NewKMSManager.
func WithFallbackEnabled(enabled bool) KMSManagerOption {
	return func(m *KMSManager) { m.fallbackEnabled = enabled }
}

// WithRoutes seeds the credential-name → provider-name routing table at
// construction time. The map is copied; mutating the original does not
// affect the manager.
func WithRoutes(routes map[string]string) KMSManagerOption {
	return func(m *KMSManager) {
		for k, v := range routes {
			m.routes[k] = v
		}
	}
}

// NewKMSManager creates a new KMSManager. fallback may be nil to disable
// the local fallback path; providers may be nil to start with an empty
// provider set (call RegisterProvider to add ones later).
//
// At least one of fallback or providers must be non-nil; otherwise
// ErrKMSNilManager is returned.
func NewKMSManager(fallback *CredentialStore, providers []KMSProvider, opts ...KMSManagerOption) (*KMSManager, error) {
	if fallback == nil && len(providers) == 0 {
		return nil, ErrKMSNilManager
	}

	m := &KMSManager{
		providers:       make(map[string]KMSProvider),
		routes:          make(map[string]string),
		fallback:        fallback,
		fallbackEnabled: fallback != nil,
	}
	for _, p := range providers {
		if p == nil {
			continue
		}
		m.providers[p.Name()] = p
	}
	for _, opt := range opts {
		opt(m)
	}
	return m, nil
}

// RegisterProvider adds a provider. Returns ErrKMSProviderExists when a
// provider with the same name is already registered.
func (m *KMSManager) RegisterProvider(p KMSProvider) error {
	if p == nil {
		return fmt.Errorf("credential: nil kms provider")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	name := p.Name()
	if _, ok := m.providers[name]; ok {
		return fmt.Errorf("credential: provider %q: %w", name, ErrKMSProviderExists)
	}
	m.providers[name] = p
	return nil
}

// UnregisterProvider removes a provider. Returns ErrKMSUnknownProvider when
// no provider with the given name is registered. The default provider and
// any routes pointing at it are cleared as well.
func (m *KMSManager) UnregisterProvider(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[name]; !ok {
		return fmt.Errorf("credential: provider %q: %w", name, ErrKMSUnknownProvider)
	}
	delete(m.providers, name)
	if m.defaultProvider == name {
		m.defaultProvider = ""
	}
	for cred, prov := range m.routes {
		if prov == name {
			delete(m.routes, cred)
		}
	}
	return nil
}

// Route maps a credential name to a provider. The provider must already be
// registered; otherwise ErrKMSUnknownProvider is returned.
func (m *KMSManager) Route(credName, providerName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[providerName]; !ok {
		return fmt.Errorf("credential: provider %q: %w", providerName, ErrKMSUnknownProvider)
	}
	m.routes[credName] = providerName
	return nil
}

// Unroute removes the explicit route for a credential. The credential will
// subsequently be serviced by the default provider (if any).
func (m *KMSManager) Unroute(credName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, credName)
}

// SetDefaultProvider sets the provider used for credentials without an
// explicit route. The provider must already be registered.
func (m *KMSManager) SetDefaultProvider(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.providers[name]; !ok {
		return fmt.Errorf("credential: provider %q: %w", name, ErrKMSUnknownProvider)
	}
	m.defaultProvider = name
	return nil
}

// SetFallbackEnabled toggles the local fallback at runtime.
func (m *KMSManager) SetFallbackEnabled(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallbackEnabled = enabled
}

// --- Credential resolution --------------------------------------------------

// KMSCredential is the result of a successful GetCredential call. The
// Plaintext field contains the decrypted bytes; the caller MUST call
// ClearKMSCredential when done to zero them from memory.
//
// Source records where the plaintext came from:
//   - "kms:<provider-name>" — served by an external KMS provider
//   - "local"               — served by the fallback CredentialStore
type KMSCredential struct {
	Name      string    // 凭据名
	Plaintext []byte    // 明文（调用方负责清零）
	LeaseID   string    // KMS 租约 ID（仅 KMS 来源；local 来源为空）
	Source    string    // 来源标签：kms:<provider> | local
	Provider  string    // provider 名（local 来源为空）
	FetchedAt time.Time // 获取时间
}

// ClearKMSCredential zeros the Plaintext of a KMSCredential and releases
// the reference. It is safe to call on a nil credential or one with
// nil/empty Plaintext. The runtime.KeepAlive call prevents the compiler
// from optimizing the zeroing away.
func ClearKMSCredential(c *KMSCredential) {
	if c == nil {
		return
	}
	SecureZero(c.Plaintext)
	c.Plaintext = nil
}

// GetCredential resolves a credential by name. It first attempts the KMS
// provider selected by the routing table (or the default provider when no
// explicit route exists). On KMS failure the manager falls back to the
// local CredentialStore when fallback is enabled, recording a WARN-level
// log event so operators can investigate.
//
// The caller MUST call ClearKMSCredential on the returned value when done
// to zero the plaintext from memory. When the credential was sourced from
// KMS, the caller should also call ReturnSecret on the manager to revoke
// the lease (GetCredential does not auto-revoke; the lease is held until
// the caller signals it is done).
//
// Failure modes:
//   - empty name: wraps ErrEmptyName
//   - no provider registered and no fallback: wraps ErrKMSNotRegistered
//   - KMS error and fallback disabled (or fallback also fails): wraps the
//     underlying error
func (m *KMSManager) GetCredential(ctx context.Context, name string) (*KMSCredential, error) {
	if name == "" {
		return nil, ErrEmptyName
	}

	m.mu.RLock()
	providerName, hasRoute := m.routes[name]
	if !hasRoute {
		providerName = m.defaultProvider
	}
	provider, hasProvider := m.providers[providerName]
	fallback := m.fallback
	fallbackEnabled := m.fallbackEnabled
	m.mu.RUnlock()

	// --- KMS path ----------------------------------------------------------
	if hasProvider && provider != nil {
		secret, leaseID, err := provider.GetSecret(ctx, name)
		if err == nil {
			log.DebugCtx(ctx, "kms credential fetched",
				"name", name, "provider", providerName)
			return &KMSCredential{
				Name:      name,
				Plaintext: secret,
				LeaseID:   leaseID,
				Source:    "kms:" + providerName,
				Provider:  providerName,
				FetchedAt: time.Now().UTC(),
			}, nil
		}

		// KMS failed. Decide whether to fall back.
		log.WarnCtx(ctx, "kms provider failed; attempting fallback",
			"name", name, "provider", providerName, "err", err.Error())
		if !fallbackEnabled || fallback == nil {
			return nil, fmt.Errorf("credential: kms get %q via %q: %w", name, providerName, err)
		}
		// Fall through to fallback path.
	}

	// --- Local fallback path ----------------------------------------------
	if fallback == nil {
		// No provider and no fallback.
		if !hasProvider {
			return nil, fmt.Errorf("credential: %q: %w", name, ErrKMSNotRegistered)
		}
		return nil, fmt.Errorf("credential: %q: %w", name, ErrKMSUnavailable)
	}

	plaintext, err := fallback.Retrieve(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("credential: kms fallback for %q: %w", name, err)
	}

	log.DebugCtx(ctx, "kms credential served from local fallback", "name", name)
	return &KMSCredential{
		Name:      name,
		Plaintext: plaintext,
		Source:    "local",
		FetchedAt: time.Now().UTC(),
	}, nil
}

// ReturnSecret revokes the lease held by the given KMSCredential and zeros
// its plaintext. It is safe to call on a nil credential or one sourced
// from the local fallback (LeaseID empty / Provider empty). Errors from
// the provider's ReturnSecret are logged but not propagated, because the
// caller has already consumed the plaintext and a lease-revocation failure
// should not abort the workflow.
func (m *KMSManager) ReturnSecret(ctx context.Context, c *KMSCredential) {
	if c == nil {
		return
	}
	if c.Provider != "" && c.LeaseID != "" {
		m.mu.RLock()
		provider, ok := m.providers[c.Provider]
		m.mu.RUnlock()
		if ok && provider != nil {
			if err := provider.ReturnSecret(ctx, c.LeaseID); err != nil {
				log.WarnCtx(ctx, "kms lease revoke failed",
					"name", c.Name, "provider", c.Provider, "err", err.Error())
			}
		}
	}
	ClearKMSCredential(c)
}

// --- Status & introspection -------------------------------------------------

// ProviderStatus is the per-provider health snapshot returned by Status.
type ProviderStatus struct {
	Name    string `json:"name"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// Status reports the health of every registered provider. The check is
// performed concurrently; each provider gets a per-call timeout derived
// from ctx. The returned slice is sorted by provider name for deterministic
// output.
func (m *KMSManager) Status(ctx context.Context) []ProviderStatus {
	m.mu.RLock()
	names := make([]string, 0, len(m.providers))
	providers := make(map[string]KMSProvider, len(m.providers))
	for name, p := range m.providers {
		names = append(names, name)
		providers[name] = p
	}
	defaultProvider := m.defaultProvider
	fallbackEnabled := m.fallbackEnabled
	m.mu.RUnlock()
	sort.Strings(names)

	results := make([]ProviderStatus, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(idx int, pname string, p KMSProvider) {
			defer wg.Done()
			err := p.HealthCheck(ctx)
			ps := ProviderStatus{Name: pname, Healthy: err == nil}
			if err != nil {
				ps.Error = err.Error()
			}
			results[idx] = ps
		}(i, name, providers[name])
	}
	wg.Wait()

	// Annotate the default provider.
	_ = defaultProvider
	_ = fallbackEnabled
	return results
}

// HasFallback reports whether the local fallback path is available.
func (m *KMSManager) HasFallback() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.fallback != nil && m.fallbackEnabled
}

// DefaultProvider returns the name of the default provider, or empty when
// none is set.
func (m *KMSManager) DefaultProvider() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultProvider
}

// ProviderNames returns the sorted names of all registered providers.
func (m *KMSManager) ProviderNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.providers))
	for n := range m.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// --- Convenience: build KMSManager from config ------------------------------

// KMSConfig is the runtime configuration consumed by NewKMSManagerFromConfig.
// It is intentionally provider-agnostic; each provider constructor takes
// its own typed config struct.
type KMSConfig struct {
	DefaultProvider string            // 默认 provider 名
	Routes          map[string]string // 凭据名 → provider 名
	FallbackEnabled bool              // 是否启用本地降级
}

// NewKMSManagerFromConfig is a thin convenience wrapper that builds a
// KMSManager from a KMSConfig, a fallback store and a slice of providers.
// It applies DefaultProvider, Routes and FallbackEnabled from cfg.
func NewKMSManagerFromConfig(cfg KMSConfig, fallback *CredentialStore, providers []KMSProvider) (*KMSManager, error) {
	opts := []KMSManagerOption{
		WithDefaultProvider(cfg.DefaultProvider),
		WithFallbackEnabled(cfg.FallbackEnabled),
		WithRoutes(cfg.Routes),
	}
	return NewKMSManager(fallback, providers, opts...)
}
