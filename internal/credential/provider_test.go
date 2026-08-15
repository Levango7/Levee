package credential

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestProvider returns a fresh CredentialProvider backed by a real
// SQLite store. The master password is fixed; each test gets its own
// temp database via newTestStore.
func newTestProvider(t *testing.T) *CredentialProvider {
	t.Helper()
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)
	cp, err := NewCredentialProvider(cs, nil)
	require.NoError(t, err)
	return cp
}

// storeCredential stores a credential in the provider's backing store and
// returns the plaintext copy for assertion. The CredentialStore.Store
// zeros the plaintext, so we pass a copy.
func storeCredential(t *testing.T, cp *CredentialProvider, name, credType string, plaintext string) {
	t.Helper()
	ctx := context.Background()
	pt := make([]byte, len(plaintext))
	copy(pt, plaintext)
	spec := CredentialSpec{
		Name:      name,
		Type:      credType,
		Plaintext: pt,
	}
	_, err := cp.store.Store(ctx, spec)
	require.NoError(t, err)
}

// =========================================================================
// NewCredentialProvider
// =========================================================================

func TestNewCredentialProvider(t *testing.T) {
	t.Run("ok with nil map", func(t *testing.T) {
		cp := newTestProvider(t)
		require.NotNil(t, cp)
		assert.Empty(t, cp.ListTargets())
	})

	t.Run("ok with initial map", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStore(store, "master-pass")
		require.NoError(t, err)
		initial := map[string]string{
			"host-a": "cred-a",
			"host-b": "cred-b",
		}
		cp, err := NewCredentialProvider(cs, initial)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"host-a", "host-b"}, cp.ListTargets())
	})

	t.Run("nil store returns ErrNilStore", func(t *testing.T) {
		_, err := NewCredentialProvider(nil, nil)
		assert.ErrorIs(t, err, ErrNilStore)
	})

	t.Run("initial map is copied", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStore(store, "master-pass")
		require.NoError(t, err)
		initial := map[string]string{"host-a": "cred-a"}
		cp, err := NewCredentialProvider(cs, initial)
		require.NoError(t, err)
		// Mutate the original; the provider should be unaffected.
		initial["host-a"] = "changed"
		initial["host-b"] = "cred-b"
		ctx := context.Background()
		// Register a credential so Get can proceed past the map lookup.
		storeCredential(t, cp, "cred-a", "ssh_password", "secret")
		cred, err := cp.Get(ctx, "host-a")
		require.NoError(t, err)
		assert.Equal(t, "cred-a", cred.Name)
	})
}

// =========================================================================
// Get — success
// =========================================================================

func TestGet_Success(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-ssh", "ssh_password", "super-secret")
	cp.RegisterTarget("host-prod", "prod-ssh")

	cred, err := cp.Get(ctx, "host-prod")
	require.NoError(t, err)
	require.NotNil(t, cred)

	assert.Equal(t, "host-prod", cred.Target)
	assert.Equal(t, "prod-ssh", cred.Name)
	assert.Equal(t, "ssh_password", cred.Type)
	assert.Equal(t, []byte("super-secret"), cred.Plaintext)
	assert.False(t, cred.ResolvedAt.IsZero())

	// Clean up.
	cp.Clear(cred)
}

// =========================================================================
// Get — target not registered
// =========================================================================

func TestGet_TargetNotRegistered(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	_, err := cp.Get(ctx, "host-unknown")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetNotRegistered)
	// Error message must contain the target name (R7 structured failure).
	assert.Contains(t, err.Error(), "host-unknown")
}

func TestGet_EmptyTarget(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	_, err := cp.Get(ctx, "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetNotRegistered)
}

// =========================================================================
// Get — credential not found
// =========================================================================

func TestGet_CredentialNotFound(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	// Register a mapping but never store the credential.
	cp.RegisterTarget("host-a", "missing-cred")

	_, err := cp.Get(ctx, "host-a")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCredentialNotFound)
	assert.Contains(t, err.Error(), "host-a")
}

// =========================================================================
// Clear — zeros plaintext
// =========================================================================

func TestClear_ZerosPlaintext(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-ssh", "ssh_password", "super-secret")
	cp.RegisterTarget("host-prod", "prod-ssh")

	cred, err := cp.Get(ctx, "host-prod")
	require.NoError(t, err)
	require.False(t, allZero(cred.Plaintext))

	cp.Clear(cred)
	assert.Nil(t, cred.Plaintext)
}

func TestClear_NilCredential(t *testing.T) {
	cp := newTestProvider(t)
	// Should not panic.
	cp.Clear(nil)
}

func TestClear_AlreadyCleared(t *testing.T) {
	cp := newTestProvider(t)
	cred := &ResolvedCredential{Target: "host", Name: "c", Plaintext: nil}
	// Should not panic.
	cp.Clear(cred)
	assert.Nil(t, cred.Plaintext)
}

// =========================================================================
// RegisterTarget / UnregisterTarget
// =========================================================================

func TestRegisterTarget_Overwrite(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "cred-a", "ssh_password", "secret-a")
	storeCredential(t, cp, "cred-b", "ssh_password", "secret-b")

	cp.RegisterTarget("host", "cred-a")
	cred, err := cp.Get(ctx, "host")
	require.NoError(t, err)
	assert.Equal(t, "cred-a", cred.Name)
	cp.Clear(cred)

	// Overwrite the mapping.
	cp.RegisterTarget("host", "cred-b")
	cred, err = cp.Get(ctx, "host")
	require.NoError(t, err)
	assert.Equal(t, "cred-b", cred.Name)
	cp.Clear(cred)
}

func TestUnregisterTarget(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	cp.RegisterTarget("host-a", "cred-a")
	assert.Contains(t, cp.ListTargets(), "host-a")

	cp.UnregisterTarget("host-a")
	assert.NotContains(t, cp.ListTargets(), "host-a")

	_, err := cp.Get(ctx, "host-a")
	assert.ErrorIs(t, err, ErrTargetNotRegistered)
}

func TestUnregisterTarget_NotRegistered(t *testing.T) {
	cp := newTestProvider(t)
	// Should be a no-op, not an error.
	cp.UnregisterTarget("never-existed")
	assert.Empty(t, cp.ListTargets())
}

// =========================================================================
// ListTargets
// =========================================================================

func TestListTargets_Sorted(t *testing.T) {
	cp := newTestProvider(t)

	cp.RegisterTarget("zeta", "cred-z")
	cp.RegisterTarget("alpha", "cred-a")
	cp.RegisterTarget("mango", "cred-m")

	targets := cp.ListTargets()
	assert.Equal(t, []string{"alpha", "mango", "zeta"}, targets)
}

func TestListTargets_Empty(t *testing.T) {
	cp := newTestProvider(t)
	assert.Empty(t, cp.ListTargets())
}

func TestListTargets_ReturnsCopy(t *testing.T) {
	cp := newTestProvider(t)
	cp.RegisterTarget("host", "cred")

	targets := cp.ListTargets()
	targets[0] = "mutated"

	// Internal state should be unaffected.
	assert.Contains(t, cp.ListTargets(), "host")
}

// =========================================================================
// GetByType
// =========================================================================

func TestGetByType_Success(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-key", "ssh_key", "-----BEGIN KEY-----")
	cp.RegisterTarget("host-prod", "prod-key")

	cred, err := cp.GetByType(ctx, "host-prod", "ssh_key")
	require.NoError(t, err)
	assert.Equal(t, "ssh_key", cred.Type)
	cp.Clear(cred)
}

func TestGetByType_Mismatch(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-ssh", "ssh_password", "secret")
	cp.RegisterTarget("host-prod", "prod-ssh")

	cred, err := cp.GetByType(ctx, "host-prod", "ssh_key")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTypeMismatch)
	// On mismatch the credential must be cleared, so cred is nil.
	assert.Nil(t, cred)
}

func TestGetByType_TargetNotRegistered(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	_, err := cp.GetByType(ctx, "unknown", "ssh_key")
	assert.ErrorIs(t, err, ErrTargetNotRegistered)
}

// =========================================================================
// Multiple targets — different credentials
// =========================================================================

func TestGet_MultipleTargets(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "cred-host-a", "ssh_password", "pass-a")
	storeCredential(t, cp, "cred-host-b", "ssh_password", "pass-b")
	storeCredential(t, cp, "cred-host-c", "ssh_key", "key-c")

	cp.RegisterTarget("host-a", "cred-host-a")
	cp.RegisterTarget("host-b", "cred-host-b")
	cp.RegisterTarget("host-c", "cred-host-c")

	credA, err := cp.Get(ctx, "host-a")
	require.NoError(t, err)
	assert.Equal(t, []byte("pass-a"), credA.Plaintext)

	credB, err := cp.Get(ctx, "host-b")
	require.NoError(t, err)
	assert.Equal(t, []byte("pass-b"), credB.Plaintext)

	credC, err := cp.Get(ctx, "host-c")
	require.NoError(t, err)
	assert.Equal(t, []byte("key-c"), credC.Plaintext)
	assert.Equal(t, "ssh_key", credC.Type)

	cp.Clear(credA)
	cp.Clear(credB)
	cp.Clear(credC)
}

// =========================================================================
// Use-and-discard flow: Get → use → Clear
// =========================================================================

func TestUseAndDiscardFlow(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-ssh", "ssh_password", "my-secret-password")
	cp.RegisterTarget("host-prod", "prod-ssh")

	// 1. Get
	cred, err := cp.Get(ctx, "host-prod")
	require.NoError(t, err)
	require.False(t, allZero(cred.Plaintext))

	// 2. Use — simulate by reading the plaintext.
	used := string(cred.Plaintext)
	assert.Equal(t, "my-secret-password", used)

	// 3. Clear
	cp.Clear(cred)
	assert.Nil(t, cred.Plaintext)

	// 4. Subsequent Get returns fresh plaintext (store still has it).
	cred2, err := cp.Get(ctx, "host-prod")
	require.NoError(t, err)
	assert.Equal(t, []byte("my-secret-password"), cred2.Plaintext)
	cp.Clear(cred2)
}

// =========================================================================
// Error wrapping — R7 structured failure
// =========================================================================

func TestErrorWrapping_ContainsTarget(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	// Not registered.
	_, err := cp.Get(ctx, "web-server-01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web-server-01")
	assert.True(t, strings.HasPrefix(err.Error(), "credential: resolve failed for target"),
		"error should have R7 prefix, got: %s", err.Error())

	// Registered but credential missing.
	cp.RegisterTarget("db-server-02", "no-such-cred")
	_, err = cp.Get(ctx, "db-server-02")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db-server-02")
	assert.True(t, strings.HasPrefix(err.Error(), "credential: resolve failed for target"),
		"error should have R7 prefix, got: %s", err.Error())
}

// =========================================================================
// GetMetadata (CredentialStore helper added in this file)
// =========================================================================

func TestGetMetadata(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	storeCredential(t, cp, "prod-ssh", "ssh_password", "secret")

	meta, err := cp.store.GetMetadata(ctx, "prod-ssh")
	require.NoError(t, err)
	assert.Equal(t, "prod-ssh", meta.Name)
	assert.Equal(t, "ssh_password", meta.Type)
	assert.NotEmpty(t, meta.ID)
}

func TestGetMetadata_NotFound(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	_, err := cp.store.GetMetadata(ctx, "missing")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetMetadata_EmptyName(t *testing.T) {
	cp := newTestProvider(t)
	ctx := context.Background()

	_, err := cp.store.GetMetadata(ctx, "")
	assert.ErrorIs(t, err, ErrEmptyName)
}
