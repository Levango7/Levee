package credential

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
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

// fastTimeCost / fastMemoryCost / fastParallelism are reduced argon2id
// parameters used by the test suite under -short to keep CI fast. The
// OWASP 2024 production defaults (194 MiB) are still exercised by the
// non-short test run.
const (
	fastTimeCost    uint32 = 1
	fastMemoryCost  uint32 = 8 * 1024 // 8 MiB — well above 8*parallelism
	fastParallelism uint8  = 1
)

// newTestCredentialStore builds a CredentialStore for tests. Under -short
// it uses reduced argon2id parameters so the suite stays sub-second;
// otherwise it uses the production OWASP 2024 defaults via NewCredentialStore.
func newTestCredentialStore(t *testing.T, store state.Store, masterPassword string) *CredentialStore {
	t.Helper()
	if testing.Short() {
		cs, err := NewCredentialStoreWithParams(store, masterPassword, fastTimeCost, fastMemoryCost, fastParallelism)
		require.NoError(t, err)
		return cs
	}
	cs, err := NewCredentialStore(store, masterPassword)
	require.NoError(t, err)
	return cs
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

	// Verify the OWASP 2024 default memory cost is wired in.
	t.Run("default memory cost is 194 MiB", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStore(store, "master-pass")
		require.NoError(t, err)
		assert.Equal(t, uint32(194*1024), cs.memoryCost,
			"default memoryCost must match OWASP 2024 (194 MiB = 198656 KiB)")
		assert.Equal(t, defaultTimeCost, cs.timeCost)
		assert.Equal(t, defaultParallelism, cs.parallelism)
	})
}

// =========================================================================
// NewCredentialStoreWithParams
// =========================================================================

func TestNewCredentialStoreWithParams(t *testing.T) {
	t.Run("ok with custom params", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStoreWithParams(store, "master-pass", 2, 32*1024, 2)
		require.NoError(t, err)
		require.NotNil(t, cs)
		assert.Equal(t, uint32(2), cs.timeCost)
		assert.Equal(t, uint32(32*1024), cs.memoryCost)
		assert.Equal(t, uint8(2), cs.parallelism)
	})

	t.Run("all zero falls back to defaults", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStoreWithParams(store, "master-pass", 0, 0, 0)
		require.NoError(t, err)
		assert.Equal(t, defaultTimeCost, cs.timeCost)
		assert.Equal(t, defaultMemoryCost, cs.memoryCost)
		assert.Equal(t, defaultParallelism, cs.parallelism)
	})

	t.Run("empty password returns ErrEmptyMasterPassword", func(t *testing.T) {
		store := newTestStore(t)
		_, err := NewCredentialStoreWithParams(store, "", 3, 64*1024, 4)
		assert.ErrorIs(t, err, ErrEmptyMasterPassword)
	})

	t.Run("nil store returns error", func(t *testing.T) {
		_, err := NewCredentialStoreWithParams(nil, "master-pass", 3, 64*1024, 4)
		assert.Error(t, err)
	})

	t.Run("zero timeCost returns error", func(t *testing.T) {
		store := newTestStore(t)
		_, err := NewCredentialStoreWithParams(store, "master-pass", 0, 64*1024, 4)
		assert.Error(t, err)
	})

	t.Run("zero parallelism returns error", func(t *testing.T) {
		store := newTestStore(t)
		_, err := NewCredentialStoreWithParams(store, "master-pass", 3, 64*1024, 0)
		assert.Error(t, err)
	})

	t.Run("memoryCost below 8*parallelism returns error", func(t *testing.T) {
		store := newTestStore(t)
		// parallelism=4 requires memoryCost >= 32; 16 is too low.
		_, err := NewCredentialStoreWithParams(store, "master-pass", 3, 16, 4)
		assert.Error(t, err)
	})

	t.Run("round trip with custom params", func(t *testing.T) {
		store := newTestStore(t)
		cs, err := NewCredentialStoreWithParams(store, "master-pass", 1, 8*1024, 1)
		require.NoError(t, err)

		ctx := context.Background()
		spec := CredentialSpec{
			Name:      "custom-cred",
			Type:      "api_token",
			Plaintext: []byte("custom-secret"),
		}
		_, err = cs.Store(ctx, spec)
		require.NoError(t, err)

		got, err := cs.Retrieve(ctx, "custom-cred")
		require.NoError(t, err)
		assert.Equal(t, []byte("custom-secret"), got)
		SecureZero(got)
	})
}

// =========================================================================
// Store + Retrieve round trip
// =========================================================================

func TestStoreRetrieve_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

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
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	plaintext := []byte("super-secret-password")
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: plaintext,
	}

	_, err := cs.Store(ctx, spec)
	require.NoError(t, err)

	// The plaintext slice backing array must be zeroed after Store.
	assert.True(t, allZero(plaintext), "plaintext not zeroed after Store")
}

// =========================================================================
// Retrieve not found / empty name
// =========================================================================

func TestRetrieve_NotFound(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	_, err := cs.Retrieve(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRetrieve_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	_, err := cs.Retrieve(ctx, "")
	assert.ErrorIs(t, err, ErrEmptyName)
}

// =========================================================================
// Delete
// =========================================================================

func TestDelete(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err := cs.Store(ctx, spec)
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
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	err := cs.Delete(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestDelete_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	err := cs.Delete(ctx, "")
	assert.ErrorIs(t, err, ErrEmptyName)
}

// =========================================================================
// List
// =========================================================================

func TestList(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

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
	cs := newTestCredentialStore(t, store, "master-pass")

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
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("old-password"),
	}
	_, err := cs.Store(ctx, spec)
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
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	_, err := cs.Rotate(ctx, "nonexistent", []byte("new"))
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRotate_EmptyName(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	_, err := cs.Rotate(ctx, "", []byte("new"))
	assert.ErrorIs(t, err, ErrEmptyName)
}

func TestRotate_EmptyPlaintext(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	// Pre-create the credential so we reach the plaintext check.
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("old"),
	}
	_, err := cs.Store(ctx, spec)
	require.NoError(t, err)

	_, err = cs.Rotate(ctx, "prod-ssh", []byte{})
	assert.ErrorIs(t, err, ErrEmptyPlaintext)
}

// =========================================================================
// Wrong master password
// =========================================================================

func TestWrongMasterPassword(t *testing.T) {
	store := newTestStore(t)
	cs1 := newTestCredentialStore(t, store, "correct-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err := cs1.Store(ctx, spec)
	require.NoError(t, err)

	// Use a different CredentialStore with the wrong password.
	cs2 := newTestCredentialStore(t, store, "wrong-pass")

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

	// Large slice: verify every byte is wiped. This exercises the loop
	// that the compiler might otherwise consider a dead store; the
	// runtime.KeepAlive inside SecureZero prevents that optimization.
	large := make([]byte, 4096)
	for i := range large {
		large[i] = byte(i % 256)
	}
	SecureZero(large)
	assert.True(t, allZero(large), "large buffer not zeroed")

	// Repeated zeroing must be idempotent.
	SecureZero(large)
	assert.True(t, allZero(large), "idempotent re-zero failed")

	// Zeroing a sub-slice must not corrupt bytes outside the sub-slice.
	full := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	sub := full[2:6] // {3, 4, 5, 6}
	SecureZero(sub)
	assert.Equal(t, []byte{1, 2, 0, 0, 0, 0, 7, 8}, full,
		"SecureZero corrupted bytes outside the sub-slice")
}

// =========================================================================
// Multiple credentials
// =========================================================================

func TestMultipleCredentials(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

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
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err := cs.Store(ctx, spec)
	assert.ErrorIs(t, err, ErrEmptyName)
}

func TestStore_EmptyPlaintext(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte{},
	}
	_, err := cs.Store(ctx, spec)
	assert.ErrorIs(t, err, ErrEmptyPlaintext)
}

// =========================================================================
// Ciphertext independence (different salts → different ciphertexts)
// =========================================================================

func TestCiphertextIndependence(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

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

// =========================================================================
// RotateMasterPassword
// =========================================================================

func TestRotateMasterPassword_Success(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "old-master-pass")

	ctx := context.Background()

	// Store several credentials.
	secrets := map[string]string{
		"ssh-key":   "ssh-secret",
		"api-token": "token-secret",
		"winrm-pw":  "winrm-secret",
	}
	for name, secret := range secrets {
		spec := CredentialSpec{
			Name:      name,
			Type:      "generic",
			Plaintext: []byte(secret),
		}
		_, err := cs.Store(ctx, spec)
		require.NoError(t, err)
	}

	// Rotate master password.
	count, err := cs.RotateMasterPassword(ctx, "old-master-pass", "new-master-pass")
	require.NoError(t, err)
	assert.Equal(t, 3, count)

	// Verify all credentials can still be decrypted with the new master password.
	for name, secret := range secrets {
		got, err := cs.Retrieve(ctx, name)
		require.NoError(t, err, "failed to retrieve %q after master password rotation", name)
		assert.Equal(t, []byte(secret), got, "plaintext mismatch for %q after rotation", name)
		SecureZero(got)
	}
}

func TestRotateMasterPassword_WrongOldPassword(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "correct-pass")

	ctx := context.Background()
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: []byte("secret"),
	}
	_, err := cs.Store(ctx, spec)
	require.NoError(t, err)

	_, err = cs.RotateMasterPassword(ctx, "wrong-pass", "new-pass")
	assert.ErrorIs(t, err, ErrMasterPasswordMismatch)
}

func TestRotateMasterPassword_EmptyPassword(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "master-pass")

	ctx := context.Background()

	t.Run("empty old password", func(t *testing.T) {
		_, err := cs.RotateMasterPassword(ctx, "", "new-pass")
		assert.ErrorIs(t, err, ErrEmptyMasterPassword)
	})

	t.Run("empty new password", func(t *testing.T) {
		_, err := cs.RotateMasterPassword(ctx, "master-pass", "")
		assert.ErrorIs(t, err, ErrEmptyMasterPassword)
	})

	t.Run("both empty", func(t *testing.T) {
		_, err := cs.RotateMasterPassword(ctx, "", "")
		assert.ErrorIs(t, err, ErrEmptyMasterPassword)
	})
}

func TestRotateMasterPassword_NoCredentials(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "old-pass")

	ctx := context.Background()

	// No credentials stored; rotation should still succeed and update the
	// in-memory master password.
	count, err := cs.RotateMasterPassword(ctx, "old-pass", "new-pass")
	require.NoError(t, err)
	assert.Equal(t, 0, count)

	// Store a new credential after rotation; it should be encrypted with
	// the new master password.
	spec := CredentialSpec{
		Name:      "after-rotation",
		Type:      "api_token",
		Plaintext: []byte("new-secret"),
	}
	_, err = cs.Store(ctx, spec)
	require.NoError(t, err)

	got, err := cs.Retrieve(ctx, "after-rotation")
	require.NoError(t, err)
	assert.Equal(t, []byte("new-secret"), got)
	SecureZero(got)
}

func TestRotateMasterPassword_DecryptWithNewPassword(t *testing.T) {
	store := newTestStore(t)
	cs := newTestCredentialStore(t, store, "original-pass")

	ctx := context.Background()
	plaintext := []byte("top-secret-data")
	spec := CredentialSpec{
		Name:      "prod-ssh",
		Type:      "ssh_password",
		Plaintext: plaintext,
	}
	_, err := cs.Store(ctx, spec)
	require.NoError(t, err)

	// Rotate master password.
	count, err := cs.RotateMasterPassword(ctx, "original-pass", "rotated-pass")
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Retrieve with the same CredentialStore instance (now using new password)
	// should succeed.
	got, err := cs.Retrieve(ctx, "prod-ssh")
	require.NoError(t, err)
	assert.Equal(t, []byte("top-secret-data"), got)
	SecureZero(got)

	// A new CredentialStore with the OLD password should fail to decrypt.
	csOld := newTestCredentialStore(t, store, "original-pass")
	_, err = csOld.Retrieve(ctx, "prod-ssh")
	assert.ErrorIs(t, err, ErrDecryptFailed,
		"old master password should no longer decrypt after rotation")

	// A new CredentialStore with the NEW password should succeed.
	csNew := newTestCredentialStore(t, store, "rotated-pass")
	got2, err := csNew.Retrieve(ctx, "prod-ssh")
	require.NoError(t, err)
	assert.Equal(t, []byte("top-secret-data"), got2)
	SecureZero(got2)
}
