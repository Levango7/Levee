package credential

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockKMSProvider -------------------------------------------------------

// mockKMSProvider is a configurable KMSProvider implementation for tests.
type mockKMSProvider struct {
	name string

	mu sync.Mutex

	// getSecretFn is the function called by GetSecret. When nil, the
	// default behaviour returns the entry from secrets[name].
	getSecretFn func(ctx context.Context, name string) ([]byte, string, error)

	// secrets maps name → plaintext.
	secrets map[string][]byte

	// returnSecretFn is the function called by ReturnSecret.
	returnSecretFn func(ctx context.Context, leaseID string) error

	// revokedLeases tracks the lease IDs passed to ReturnSecret.
	revokedLeases []string

	// healthErr is returned by HealthCheck. nil = healthy.
	healthErr error

	// getCalls counts GetSecret invocations.
	getCalls int
}

func newMockKMSProvider(name string) *mockKMSProvider {
	return &mockKMSProvider{
		name:    name,
		secrets: make(map[string][]byte),
	}
}

func (m *mockKMSProvider) Name() string { return m.name }

func (m *mockKMSProvider) GetSecret(ctx context.Context, name string) ([]byte, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if m.getSecretFn != nil {
		return m.getSecretFn(ctx, name)
	}
	pt, ok := m.secrets[name]
	if !ok {
		return nil, "", ErrKMSNotFound
	}
	// Return a copy so the caller can zero it without affecting our map.
	out := make([]byte, len(pt))
	copy(out, pt)
	leaseID := "lease-" + name
	return out, leaseID, nil
}

func (m *mockKMSProvider) ReturnSecret(ctx context.Context, leaseID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedLeases = append(m.revokedLeases, leaseID)
	if m.returnSecretFn != nil {
		return m.returnSecretFn(ctx, leaseID)
	}
	return nil
}

func (m *mockKMSProvider) HealthCheck(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.healthErr
}

// setSecret stores a secret in the mock provider.
func (m *mockKMSProvider) setSecret(name string, plaintext []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[name] = plaintext
}

// setHealthErr sets the error returned by HealthCheck.
func (m *mockKMSProvider) setHealthErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.healthErr = err
}

// --- newTestKMSManager -----------------------------------------------------

// newTestKMSManager returns a KMSManager backed by a real SQLite store
// (for the fallback) and the given providers.
func newTestKMSManager(t *testing.T, providers []KMSProvider) *KMSManager {
	t.Helper()
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")
	mgr, err := NewKMSManager(cs, providers)
	require.NoError(t, err)
	return mgr
}

// =========================================================================
// NewKMSManager
// =========================================================================

func TestNewKMSManager(t *testing.T) {
	t.Run("ok with fallback only", func(t *testing.T) {
		store := newTestStore(t)
		cs := newTestCredentialStore(t, store, "master-pass")
		mgr, err := NewKMSManager(cs, nil)
		require.NoError(t, err)
		require.NotNil(t, mgr)
		assert.True(t, mgr.HasFallback())
		assert.Empty(t, mgr.ProviderNames())
	})

	t.Run("ok with providers only", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr, err := NewKMSManager(nil, []KMSProvider{p})
		require.NoError(t, err)
		require.NotNil(t, mgr)
		assert.False(t, mgr.HasFallback())
		assert.Equal(t, []string{"vault"}, mgr.ProviderNames())
	})

	t.Run("nil inputs returns ErrKMSNilManager", func(t *testing.T) {
		_, err := NewKMSManager(nil, nil)
		assert.ErrorIs(t, err, ErrKMSNilManager)
	})

	t.Run("with options", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr, err := NewKMSManager(nil, []KMSProvider{p},
			WithDefaultProvider("vault"),
			WithFallbackEnabled(false),
			WithRoutes(map[string]string{"db-prod": "vault"}),
		)
		require.NoError(t, err)
		assert.Equal(t, "vault", mgr.DefaultProvider())
	})
}

// =========================================================================
// RegisterProvider / UnregisterProvider
// =========================================================================

func TestKMSManager_RegisterProvider(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		p := newMockKMSProvider("vault")
		require.NoError(t, mgr.RegisterProvider(p))
		assert.Equal(t, []string{"vault"}, mgr.ProviderNames())
	})

	t.Run("duplicate returns ErrKMSProviderExists", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		p := newMockKMSProvider("vault")
		require.NoError(t, mgr.RegisterProvider(p))
		err := mgr.RegisterProvider(newMockKMSProvider("vault"))
		assert.ErrorIs(t, err, ErrKMSProviderExists)
	})

	t.Run("nil provider returns error", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		err := mgr.RegisterProvider(nil)
		assert.Error(t, err)
	})
}

func TestKMSManager_UnregisterProvider(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.UnregisterProvider("vault"))
		assert.Empty(t, mgr.ProviderNames())
	})

	t.Run("unknown returns ErrKMSUnknownProvider", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		err := mgr.UnregisterProvider("nope")
		assert.ErrorIs(t, err, ErrKMSUnknownProvider)
	})

	t.Run("clears default and routes", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.SetDefaultProvider("vault"))
		require.NoError(t, mgr.Route("db", "vault"))
		require.NoError(t, mgr.UnregisterProvider("vault"))
		assert.Empty(t, mgr.DefaultProvider())
	})
}

// =========================================================================
// Route / SetDefaultProvider
// =========================================================================

func TestKMSManager_Route(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.Route("db-prod", "vault"))
	})

	t.Run("unknown provider returns ErrKMSUnknownProvider", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		err := mgr.Route("db-prod", "nope")
		assert.ErrorIs(t, err, ErrKMSUnknownProvider)
	})
}

func TestKMSManager_SetDefaultProvider(t *testing.T) {
	p := newMockKMSProvider("vault")
	mgr := newTestKMSManager(t, []KMSProvider{p})

	t.Run("ok", func(t *testing.T) {
		require.NoError(t, mgr.SetDefaultProvider("vault"))
		assert.Equal(t, "vault", mgr.DefaultProvider())
	})

	t.Run("unknown returns ErrKMSUnknownProvider", func(t *testing.T) {
		err := mgr.SetDefaultProvider("nope")
		assert.ErrorIs(t, err, ErrKMSUnknownProvider)
	})
}

// =========================================================================
// GetCredential — KMS path
// =========================================================================

func TestKMSManager_GetCredential_KMS(t *testing.T) {
	t.Run("routed credential", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		p.setSecret("db-prod", []byte("super-secret"))
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.Route("db-prod", "vault"))

		ctx := context.Background()
		cred, err := mgr.GetCredential(ctx, "db-prod")
		require.NoError(t, err)
		assert.Equal(t, "kms:vault", cred.Source)
		assert.Equal(t, "vault", cred.Provider)
		assert.Equal(t, []byte("super-secret"), cred.Plaintext)
		assert.NotEmpty(t, cred.LeaseID)

		// ReturnSecret should revoke the lease and zero the plaintext.
		mgr.ReturnSecret(ctx, cred)
		assert.Empty(t, cred.Plaintext)
		assert.Contains(t, p.revokedLeases, "lease-db-prod")
	})

	t.Run("default provider", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		p.setSecret("any", []byte("val"))
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.SetDefaultProvider("vault"))

		ctx := context.Background()
		cred, err := mgr.GetCredential(ctx, "any")
		require.NoError(t, err)
		assert.Equal(t, "kms:vault", cred.Source)
		ClearKMSCredential(cred)
	})

	t.Run("empty name returns ErrEmptyName", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		_, err := mgr.GetCredential(context.Background(), "")
		assert.ErrorIs(t, err, ErrEmptyName)
	})

	t.Run("not found returns error", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.SetDefaultProvider("vault"))

		_, err := mgr.GetCredential(context.Background(), "missing")
		assert.Error(t, err)
	})
}

// =========================================================================
// GetCredential — fallback path
// =========================================================================

func TestKMSManager_GetCredential_Fallback(t *testing.T) {
	t.Run("kms failure falls back to local", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		p.setHealthErr(errors.New("vault down"))
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.SetDefaultProvider("vault"))

		ctx := context.Background()
		storeCredentialForFallback(t, mgr, "fb-cred", "local-secret")

		cred, err := mgr.GetCredential(ctx, "fb-cred")
		require.NoError(t, err)
		assert.Equal(t, "local", cred.Source)
		assert.Empty(t, cred.Provider)
		assert.Equal(t, []byte("local-secret"), cred.Plaintext)
		ClearKMSCredential(cred)
	})

	t.Run("fallback disabled returns KMS error", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		p.setHealthErr(errors.New("vault down"))
		// Use a custom getSecretFn so GetSecret returns an error.
		p.getSecretFn = func(ctx context.Context, name string) ([]byte, string, error) {
			return nil, "", errors.New("vault down")
		}
		mgr := newTestKMSManager(t, []KMSProvider{p})
		require.NoError(t, mgr.SetDefaultProvider("vault"))
		mgr.SetFallbackEnabled(false)

		_, err := mgr.GetCredential(context.Background(), "any")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "vault down")
	})

	t.Run("no provider and no fallback returns ErrKMSNotRegistered", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr, err := NewKMSManager(nil, []KMSProvider{p})
		require.NoError(t, err)
		// No default provider, no route, no fallback.
		_, err = mgr.GetCredential(context.Background(), "any")
		assert.Error(t, err)
	})
}

// storeCredentialForFallback stores a credential in the manager's local
// fallback store so GetCredential can find it when the KMS path fails.
func storeCredentialForFallback(t *testing.T, mgr *KMSManager, name, plaintext string) {
	t.Helper()
	// The fallback CredentialStore is unexported, so the test reaches for
	// it directly (same package). A loaded shared runner under -race has
	// been measured at ~9s for a single Store (the argon2id KDF dominates)
	// -- a tight timeout here once expired silently while the error was
	// discarded, turning this fixture into a baffling "not found" on the
	// GetCredential that follows. Give the KDF room and fail loudly.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pt := make([]byte, len(plaintext))
	copy(pt, plaintext)
	_, err := mgr.fallback.Store(ctx, CredentialSpec{
		Name:      name,
		Type:      "test",
		Plaintext: pt,
	})
	require.NoError(t, err)
}

// =========================================================================
// ReturnSecret
// =========================================================================

func TestKMSManager_ReturnSecret(t *testing.T) {
	t.Run("nil credential is no-op", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		mgr.ReturnSecret(context.Background(), nil)
	})

	t.Run("local credential zeros plaintext", func(t *testing.T) {
		mgr := newTestKMSManager(t, nil)
		cred := &KMSCredential{
			Name:      "x",
			Plaintext: []byte("secret"),
			Source:    "local",
		}
		mgr.ReturnSecret(context.Background(), cred)
		assert.Empty(t, cred.Plaintext)
	})

	t.Run("kms credential revokes lease and zeros", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		cred := &KMSCredential{
			Name:      "x",
			Plaintext: []byte("secret"),
			Source:    "kms:vault",
			Provider:  "vault",
			LeaseID:   "lease-x",
		}
		mgr.ReturnSecret(context.Background(), cred)
		assert.Empty(t, cred.Plaintext)
		assert.Contains(t, p.revokedLeases, "lease-x")
	})
}

// =========================================================================
// Status
// =========================================================================

func TestKMSManager_Status(t *testing.T) {
	t.Run("healthy provider", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p})
		statuses := mgr.Status(context.Background())
		require.Len(t, statuses, 1)
		assert.Equal(t, "vault", statuses[0].Name)
		assert.True(t, statuses[0].Healthy)
	})

	t.Run("unhealthy provider", func(t *testing.T) {
		p := newMockKMSProvider("vault")
		p.setHealthErr(errors.New("down"))
		mgr := newTestKMSManager(t, []KMSProvider{p})
		statuses := mgr.Status(context.Background())
		require.Len(t, statuses, 1)
		assert.False(t, statuses[0].Healthy)
		assert.Contains(t, statuses[0].Error, "down")
	})

	t.Run("multiple providers sorted", func(t *testing.T) {
		p1 := newMockKMSProvider("aws-kms")
		p2 := newMockKMSProvider("vault")
		mgr := newTestKMSManager(t, []KMSProvider{p1, p2})
		statuses := mgr.Status(context.Background())
		require.Len(t, statuses, 2)
		assert.Equal(t, "aws-kms", statuses[0].Name)
		assert.Equal(t, "vault", statuses[1].Name)
	})
}

// =========================================================================
// ClearKMSCredential
// =========================================================================

func TestClearKMSCredential(t *testing.T) {
	t.Run("nil is no-op", func(t *testing.T) {
		ClearKMSCredential(nil)
	})

	t.Run("zeros plaintext", func(t *testing.T) {
		c := &KMSCredential{Plaintext: []byte("secret")}
		ClearKMSCredential(c)
		assert.Empty(t, c.Plaintext)
	})
}

// =========================================================================
// NewKMSManagerFromConfig
// =========================================================================

func TestNewKMSManagerFromConfig(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")
	p := newMockKMSProvider("vault")

	cfg := KMSConfig{
		DefaultProvider: "vault",
		Routes:          map[string]string{"db": "vault"},
		FallbackEnabled: true,
	}
	mgr, err := NewKMSManagerFromConfig(cfg, cs, []KMSProvider{p})
	require.NoError(t, err)
	assert.Equal(t, "vault", mgr.DefaultProvider())
	assert.True(t, mgr.HasFallback())
}
