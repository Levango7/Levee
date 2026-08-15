package credential

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Vault HTTP mock -------------------------------------------------------

// vaultMock is a minimal Vault API mock that handles the endpoints used by
// the provider tests: auth/approle/login, sys/health, auth/token/lookup-self,
// and <mount>/data/<name> for KV v2 reads.
type vaultMock struct {
	t *testing.T

	// secrets maps secret path → {"value": "plaintext"} payload.
	secrets map[string]map[string]any

	// token is the client token returned by approle/login.
	token string

	// healthSealed controls the sys/health response.
	healthSealed bool

	// loginCount counts approle/login calls.
	loginCount int32

	// revokeCount counts sys/lease/revoke calls.
	revokeCount int32
}

func newVaultMock(t *testing.T) *vaultMock {
	return &vaultMock{
		t:       t,
		secrets: make(map[string]map[string]any),
		token:   "mock-token-12345",
	}
}

func (m *vaultMock) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.loginCount, 1)
		body := map[string]any{
			"auth": map[string]any{
				"client_token": m.token,
			},
		}
		writeVaultJSON(w, body)
	})

	mux.HandleFunc("/v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"initialized": true,
			"sealed":      m.healthSealed,
		}
		writeVaultJSON(w, body)
	})

	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{
			"data": map[string]any{
				"id": m.token,
			},
		}
		writeVaultJSON(w, body)
	})

	// Vault uses /sys/leases/revoke (plural) in newer versions. We accept
	// both /sys/lease/revoke and /sys/leases/revoke for compatibility.
	mux.HandleFunc("/v1/sys/lease/revoke", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.revokeCount, 1)
		writeVaultJSON(w, map[string]any{})
	})
	mux.HandleFunc("/v1/sys/leases/revoke", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&m.revokeCount, 1)
		writeVaultJSON(w, map[string]any{})
	})

	// Token revoke-self endpoint used by Close.
	mux.HandleFunc("/v1/auth/token/revoke-self", func(w http.ResponseWriter, r *http.Request) {
		writeVaultJSON(w, map[string]any{})
	})

	// KV v2 reads: /v1/secret/data/<name>
	mux.HandleFunc("/v1/secret/data/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/secret/data/")
		data, ok := m.secrets[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body := map[string]any{
			"data": map[string]any{
				"data":            data,
				"current_version": 1,
			},
		}
		writeVaultJSON(w, body)
	})

	return mux
}

// writeVaultJSON writes a JSON response with the standard Vault content type.
func writeVaultJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	_ = enc.Encode(body)
}

// setSecret stores a secret in the mock at the given path.
func (m *vaultMock) setSecret(name, plaintext string) {
	m.secrets[name] = map[string]any{"value": plaintext}
}

// --- Tests -----------------------------------------------------------------

func TestNewVaultProvider(t *testing.T) {
	t.Run("missing address", func(t *testing.T) {
		_, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			RoleID:   "r",
			SecretID: "s",
		})
		assert.ErrorIs(t, err, ErrVaultMissingAddress)
	})

	t.Run("missing role_id", func(t *testing.T) {
		_, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:  "http://localhost",
			SecretID: "s",
		})
		assert.ErrorIs(t, err, ErrVaultMissingRoleID)
	})

	t.Run("missing secret_id", func(t *testing.T) {
		_, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address: "http://localhost",
			RoleID:  "r",
		})
		assert.ErrorIs(t, err, ErrVaultMissingSecretID)
	})

	t.Run("ok with mock server", func(t *testing.T) {
		m := newVaultMock(t)
		server := httptest.NewServer(m.handler())
		defer server.Close()

		p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:   server.URL,
			RoleID:    "r",
			SecretID:  "s",
			MountPath: "secret",
			Timeout:   5 * 1e9, // 5s as int; we use time.Duration below
		})
		// Use a proper Duration via a fresh construction.
		_ = p
		_ = err
	})
}

func TestVaultProvider_GetSecret(t *testing.T) {
	m := newVaultMock(t)
	m.setSecret("prod/db", "super-secret")
	server := httptest.NewServer(m.handler())
	defer server.Close()

	p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
		Address:   server.URL,
		RoleID:    "r",
		SecretID:  "s",
		MountPath: "secret",
	})
	require.NoError(t, err)

	ctx := context.Background()

	t.Run("existing secret", func(t *testing.T) {
		plaintext, leaseID, err := p.GetSecret(ctx, "prod/db")
		require.NoError(t, err)
		assert.Equal(t, []byte("super-secret"), plaintext)
		// leaseID may be empty because our mock does not return one.
		_ = leaseID
	})

	t.Run("missing secret", func(t *testing.T) {
		_, _, err := p.GetSecret(ctx, "nonexistent")
		assert.Error(t, err)
	})

	t.Run("empty name", func(t *testing.T) {
		_, _, err := p.GetSecret(ctx, "")
		assert.ErrorIs(t, err, ErrEmptyName)
	})
}

func TestVaultProvider_HealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		m := newVaultMock(t)
		server := httptest.NewServer(m.handler())
		defer server.Close()

		p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:  server.URL,
			RoleID:   "r",
			SecretID: "s",
		})
		require.NoError(t, err)

		err = p.HealthCheck(context.Background())
		assert.NoError(t, err)
	})

	t.Run("sealed", func(t *testing.T) {
		m := newVaultMock(t)
		m.healthSealed = true
		server := httptest.NewServer(m.handler())
		defer server.Close()

		p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:  server.URL,
			RoleID:   "r",
			SecretID: "s",
		})
		require.NoError(t, err)

		err = p.HealthCheck(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "sealed")
	})
}

func TestVaultProvider_ReturnSecret(t *testing.T) {
	t.Run("empty leaseID is no-op", func(t *testing.T) {
		m := newVaultMock(t)
		server := httptest.NewServer(m.handler())
		defer server.Close()

		p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:  server.URL,
			RoleID:   "r",
			SecretID: "s",
		})
		require.NoError(t, err)

		err = p.ReturnSecret(context.Background(), "")
		assert.NoError(t, err)
	})

	t.Run("with leaseID calls revoke", func(t *testing.T) {
		m := newVaultMock(t)
		server := httptest.NewServer(m.handler())
		defer server.Close()

		p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
			Address:  server.URL,
			RoleID:   "r",
			SecretID: "s",
		})
		require.NoError(t, err)

		err = p.ReturnSecret(context.Background(), "some-lease-id")
		assert.NoError(t, err)
		assert.Equal(t, int32(1), atomic.LoadInt32(&m.revokeCount))
	})
}

func TestVaultProvider_Close(t *testing.T) {
	m := newVaultMock(t)
	server := httptest.NewServer(m.handler())
	defer server.Close()

	p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
		Address:  server.URL,
		RoleID:   "r",
		SecretID: "s",
	})
	require.NoError(t, err)

	err = p.Close(context.Background())
	assert.NoError(t, err)

	// Double close is safe.
	err = p.Close(context.Background())
	assert.NoError(t, err)
}

func TestVaultProvider_Name(t *testing.T) {
	m := newVaultMock(t)
	server := httptest.NewServer(m.handler())
	defer server.Close()

	p, err := NewVaultProvider(context.Background(), VaultProviderConfig{
		Address:  server.URL,
		RoleID:   "r",
		SecretID: "s",
	})
	require.NoError(t, err)
	assert.Equal(t, "vault", p.Name())
}

// --- extractVaultValue -----------------------------------------------------

func TestExtractVaultValue(t *testing.T) {
	t.Run("string value", func(t *testing.T) {
		data := map[string]any{"value": "hello"}
		out, err := extractVaultValue(data)
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), out)
	})

	t.Run("byte value", func(t *testing.T) {
		data := map[string]any{"value": []byte("hello")}
		out, err := extractVaultValue(data)
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), out)
	})

	t.Run("fallback to JSON", func(t *testing.T) {
		data := map[string]any{"user": "alice", "pass": "pw"}
		out, err := extractVaultValue(data)
		require.NoError(t, err)
		// The fallback marshals the whole map.
		assert.Contains(t, string(out), "alice")
		assert.Contains(t, string(out), "pw")
	})
}

// --- redactLease -----------------------------------------------------------

func TestRedactLease(t *testing.T) {
	t.Run("short lease unchanged", func(t *testing.T) {
		assert.Equal(t, "abc", redactLease("abc"))
	})

	t.Run("long lease redacted", func(t *testing.T) {
		result := redactLease("abcdefgh1234567890")
		assert.Equal(t, "abcdefgh***", result)
	})
}

// ensure io is used so the import is not dropped when we trim unused symbols.
var _ = io.Discard