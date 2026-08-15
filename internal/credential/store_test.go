package credential

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/nexus/levee/internal/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore returns a fresh SQLite-backed state.Store for each test.
// Each test gets its own temp directory so concurrent tests do not collide.
func newTestStore(t *testing.T) state.Store {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-cred-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// allZero reports whether every byte in b is zero.
func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// =========================================================================
// NewCredentialStore
// =========================================================================

func TestNewCredentialStore(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStore(store, "master-pass")
		require.NoError(t, err)
		require.NotNil(t, cs)
	})

	t.Run("empty password returns ErrEmptyMasterPassword", func(t *testing.T) {
		store := newTestStore(t)
		_, err := NewCredentialStore(store, "")
		assert.ErrorIs(t, err, ErrEmptyMasterPassword)
	})

	t.Run("nil store returns error", func(t *testing.T) {
		_, err := NewCredentialStore(nil, "master-pass")
		assert.Error(t, err)
	})
}

// =========================================================================
// Store + Retrieve round trip
// =========================================================================

func TestStoreRetrieve_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	plaintext := []byte("super-secret-password")
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: plaintext,
	}

	cred, err := cs.Store(ctx, spec)
	require.NoError(t, err)
	assert.NotEmpty(t, cred.ID)
	assert.Equal(t, "prod-ssh", cred.Name)
	assert.Equal(t, "ssh_password", cred.Type)
	assert.NotEmpty(t, cred.EncryptedData)
	assert.False(t, cred.CreatedAt.IsZero())

	// Ciphertext blob must be at least salt(16) + nonce(12) + gcm tag (16).
	assert.GreaterOrEqual(t, len(cred.EncryptedData), saltLen+nonceLen+16)

	got, err := cs.Retrieve(ctx, "prod-ssh")
	require.NoError(t, err)
	assert.Equal(t, []byte("super-secret-password"), got)

	// Clean up sensitive material.
	SecureZero(got)
}

// =========================================================================
// Store zeroes plaintext
// =========================================================================

func TestStore_PlaintextZeroed(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	plaintext := []byte("super-secret-password")
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: plaintext,
	}

	_, err = cs.Store(ctx, spec)
	require.NoError(t, err)

	// The plaintext slice backing array must be zeroed after Store.
	assert.True(t, allZero(plaintext), "plaintext not zeroed after Store")
}

// =========================================================================
// Retrieve not found / empty name
// =========================================================================

func TestRetrieve_NotFound(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	_, err = cs.Retrieve(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRetrieve_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	_, err = cs.Retrieve(ctx, "")
	assert.ErrorIs(t, err, ErrEmptyName)
}

// =========================================================================
// Delete
// =========================================================================

func TestDelete(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err = cs.Store(ctx, spec)
	require.NoError(t, err)

	// Verify it exists.
	got, err := cs.Retrieve(ctx, "prod-ssh")
	require.NoError(t, err)
	SecureZero(got)

	// Delete.
	err = cs.Delete(ctx, "prod-ssh")
	require.NoError(t, err)

	// Retrieve should now return ErrNotFound.
	_, err = cs.Retrieve(ctx, "prod-ssh")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDelete_NotFound(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	err = cs.Delete(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDelete_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	err = cs.Delete(ctx, "")
	assert.ErrorIs(t, err, ErrEmptyName)
}

// =========================================================================
// List
// =========================================================================

func TestList(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		spec := CredentialSpec{
			Name:      fmt.Sprintf("cred-%d", i),
			Type:      "api_token",
			Plaintext: []byte(fmt.Sprintf("secret-%d", i)),
		}
		_, err := cs.Store(ctx, spec)
		require.NoError(t, err)
	}

	list, err := cs.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)

	// None of the returned credentials should expose plaintext. The
	// EncryptedData field is the ciphertext blob; it must not contain
	// the plaintext string.
	for _, c := range list {
		assert.NotEmpty(t, c.EncryptedData)
		assert.NotContains(t, string(c.EncryptedData), "secret-")
	}
}

func TestList_Empty(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	list, err := cs.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// =========================================================================
// Rotate
// =========================================================================

func TestRotate(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("old-password"),
	}
	_, err = cs.Store(ctx, spec)
	require.NoError(t, err)

	// Rotate.
	newPlain := []byte("new-password")
	cred, err := cs.Rotate(ctx, "prod-ssh", newPlain)
	require.NoError(t, err)
	require.NotNil(t, cred.RotatedAt)
	assert.False(t, cred.RotatedAt.IsZero())

	// The new plaintext backing array must be zeroed after Rotate.
	assert.True(t, allZero(newPlain), "new plaintext not zeroed after Rotate")

	// Retrieve should return the new plaintext.
	got, err := cs.Retrieve(ctx, "prod-ssh")
	require.NoError(t, err)
	assert.Equal(t, []byte("new-password"), got)
	SecureZero(got)
}

func TestRotate_NotFound(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	_, err = cs.Rotate(ctx, "nonexistent", []byte("new"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRotate_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	_, err = cs.Rotate(ctx, "", []byte("new"))
	assert.ErrorIs(t, err, ErrEmptyName)
}

func TestRotate_EmptyPlaintext(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	// Pre-create the credential so we reach the plaintext check.
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("old"),
	}
	_, err = cs.Store(ctx, spec)
	require.NoError(t, err)

	_, err = cs.Rotate(ctx, "prod-ssh", []byte{})
	assert.ErrorIs(t, err, ErrEmptyPlaintext)
}

// =========================================================================
// Wrong master password
// =========================================================================

func TestWrongMasterPassword(t *testing.T) {
	store := newTestStore(t)
	cs1, err := NewCredentialStore(store, "correct-pass")
	require.NoError(t, err)

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err = cs1.Store(ctx, spec)
	require.NoError(t, err)

	// Use a different CredentialStore with the wrong password.
	cs2, err := NewCredentialStore(store, "wrong-pass")
	require.NoError(t, err)

	_, err = cs2.Retrieve(ctx, "prod-ssh")
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

// =========================================================================
// SecureZero
// =========================================================================

func TestSecureZero(t *testing.T) {
	b := []byte{1, 2, 3, 4, 5}
	SecureZero(b)
	assert.True(t, allZero(b), "bytes not zeroed")

	// Empty/nil slices should be a no-op (no panic).
	SecureZero(nil)
	SecureZero([]byte{})
}

// =========================================================================
// Multiple credentials
// =========================================================================

func TestMultipleCredentials(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	creds := []struct {
		name      string
		plaintext string
	}{
		{"ssh-key-1", "key1-secret"},
		{"ssh-key-2", "key2-secret"},
		{"api-token", "token-secret"},
	}

	for _, c := range creds {
		spec := CredentialSpec{
			Name:      c.name,
			Type:      "generic",
			Plaintext: []byte(c.plaintext),
		}
		_, err := cs.Store(ctx, spec)
		require.NoError(t, err)
	}

	// Each credential should decrypt to its own plaintext.
	for _, c := range creds {
		got, err := cs.Retrieve(ctx, c.name)
		require.NoError(t, err)
		assert.Equal(t, []byte(c.plaintext), got)
		SecureZero(got)
	}

	// List should report all three.
	list, err := cs.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 3)
}

// =========================================================================
// Store validation
// =========================================================================

func TestStore_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err = cs.Store(ctx, spec)
	assert.ErrorIs(t, err, ErrEmptyName)
}

func TestStore_EmptyPlaintext(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte{},
	}
	_, err = cs.Store(ctx, spec)
	assert.ErrorIs(t, err, ErrEmptyPlaintext)
}

// =========================================================================
// Ciphertext independence (different salts → different ciphertexts)
// =========================================================================

func TestCiphertextIndependence(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStore(store, "master-pass")
	require.NoError(t, err)

	ctx := context.Background()

	// Store the same plaintext under two different names.
	spec1 := CredentialSpec{Name: "cred-a", Type: "generic", Plaintext: []byte("same-secret")}
	spec2 := CredentialSpec{Name: "cred-b", Type: "generic", Plaintext: []byte("same-secret")}

	c1, err := cs.Store(ctx, spec1)
	require.NoError(t, err)
	c2, err := cs.Store(ctx, spec2)
	require.NoError(t, err)

	// The two ciphertext blobs must differ because each uses a fresh
	// random salt and nonce. This guards against ECB-style reuse bugs.
	assert.NotEqual(t, c1.EncryptedData, c2.EncryptedData,
		"two credentials with the same plaintext must have different ciphertexts")
}
