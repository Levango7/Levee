package credential

// Blob format versioning tests (SA-005 / SA-015): the v1 self-describing
// header, the deterministic hard-fail for corrupt v1 records, and the
// authenticated parameter chain that keeps BOTH released generations of
// headless legacy blobs (≤ v1.10.0 at 64MiB, v1.11.0/v1.12.x at 194MiB)
// decryptable after the memory-cost bump.

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"

	"github.com/nexus/levee/internal/state"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	require.NoError(t, err)
	return b
}

// Golden fixtures generated OUT OF BAND with the pre-v1 code path
// (argon2id over a fixed salt/nonce, salt||nonce||seal layout):
//
//	password: "golden-master-2026-08"
//	plaintext: "golden-plaintext-LEVEE"
//	salt:      00112233445566778899aabbccddeeff
//	nonce:     0102030405060708090a0b0c
const (
	goldenPassword  = "golden-master-2026-08"
	goldenPlaintext = "golden-plaintext-LEVEE"
	// headless blob derived with t=3, m=64MiB, p=4 (released ≤ v1.10.0)
	goldenBlob64 = "00112233445566778899aabbccddeeff0102030405060708090a0b0c" +
		"7d9434d380e2bb0e9e6167d6a4c5f5b0c56fb9ec91d06ee62036a5a80abba852004531c5cd9c"
	// headless blob derived with t=3, m=194MiB, p=4 (released v1.11.0/v1.12.x)
	goldenBlob194 = "00112233445566778899aabbccddeeff0102030405060708090a0b0c" +
		"83887facdc9f42f6b34128af469b1d2e3af5418b2fb2881289a64b8d08105c57decdcb874120"
)

// seedLegacyCredential persists a credential row holding a raw pre-v1 blob.
func seedLegacyCredential(t *testing.T, store state.Store, name, hexBlob string) *state.Credential {
	t.Helper()
	cred := &state.Credential{
		ID:            "cred-golden-" + name,
		Name:          name,
		Type:          "api_token",
		EncryptedData: mustHex(t, hexBlob),
		CreatedAt:     time.Now().UTC(),
	}
	require.NoError(t, store.CreateCredential(context.Background(), cred))
	return cred
}

// =========================================================================
// v1 round-trip and header layout
// =========================================================================

func TestBlobV1RoundTripAndHeader(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStoreWithParams(store, "master-pass", 2, 32*1024, 2)
	require.NoError(t, err)

	blob, err := cs.encrypt([]byte("hello v1"))
	require.NoError(t, err)

	require.True(t, bytes.HasPrefix(blob, []byte(v1Magic)), "blob must start with the v1 magic")
	assert.Equal(t, byte(v1Version), blob[v1OffVersion])
	assert.Equal(t, byte(2), blob[v1OffTime], "header must carry the store's timeCost")
	assert.Equal(t, uint32(32*1024), binary.BigEndian.Uint32(blob[v1OffMem:v1OffPar]),
		"header must carry the store's memoryCost")
	assert.Equal(t, byte(2), blob[v1OffPar], "header must carry the store's parallelism")
	assert.GreaterOrEqual(t, len(blob), minV1BlobLen)

	pt, err := cs.decrypt(blob)
	require.NoError(t, err)
	assert.Equal(t, "hello v1", string(pt))
}

func TestBlobV1EmbeddedParamsWinOverStoreParams(t *testing.T) {
	// A v1 header pins the parameters at write time: a store configured with
	// DIFFERENT (but valid) parameters must still decrypt, deriving with the
	// embedded set. This is what makes future default bumps safe.
	store := newTestStore(t)
	writer, err := NewCredentialStoreWithParams(store, "shared-master", 1, 8*1024, 1)
	require.NoError(t, err)
	blob, err := writer.encrypt([]byte("param drift survivor"))
	require.NoError(t, err)

	reader, err := NewCredentialStoreWithParams(store, "shared-master", 3, 24*1024, 3)
	require.NoError(t, err)
	pt, err := reader.decrypt(blob)
	require.NoError(t, err)
	assert.Equal(t, "param drift survivor", string(pt))
}

// =========================================================================
// v1 hard-fail semantics (no silent legacy probing)
// =========================================================================

func TestBlobV1CorruptionHardFails(t *testing.T) {
	store := newTestStore(t)
	cs, err := NewCredentialStoreWithParams(store, "master-pass", 1, 8*1024, 1)
	require.NoError(t, err)
	blob, err := cs.encrypt([]byte("secret payload"))
	require.NoError(t, err)

	t.Run("flipped ciphertext byte -> ErrDecryptFailed", func(t *testing.T) {
		bad := bytes.Clone(blob)
		bad[len(bad)-1] ^= 0xFF
		_, err := cs.decrypt(bad)
		assert.ErrorIs(t, err, ErrDecryptFailed)
	})

	t.Run("flipped header param byte -> ErrDecryptFailed", func(t *testing.T) {
		bad := bytes.Clone(blob)
		bad[v1OffMem+3] ^= 0x01 // memoryCost drifts -> wrong key -> GCM rejects
		_, err := cs.decrypt(bad)
		assert.ErrorIs(t, err, ErrDecryptFailed)
	})

	t.Run("truncated v1 blob -> ErrInvalidCiphertext", func(t *testing.T) {
		_, err := cs.decrypt(blob[:v1HeaderLen+3])
		assert.ErrorIs(t, err, ErrInvalidCiphertext)
	})

	t.Run("structurally impossible params -> ErrInvalidCiphertext", func(t *testing.T) {
		bad := bytes.Clone(blob)
		bad[v1OffPar] = 0
		_, err := cs.decrypt(bad)
		assert.ErrorIs(t, err, ErrInvalidCiphertext)
		bad[v1OffPar] = 4
		binary.BigEndian.PutUint32(bad[v1OffMem:v1OffPar], 8) // 8 < 8*4
		_, err = cs.decrypt(bad)
		assert.ErrorIs(t, err, ErrInvalidCiphertext)
	})

	t.Run("wrong master password -> ErrDecryptFailed", func(t *testing.T) {
		other, err := NewCredentialStoreWithParams(store, "not-the-master", 1, 8*1024, 1)
		require.NoError(t, err)
		_, err = other.decrypt(blob)
		assert.ErrorIs(t, err, ErrDecryptFailed)
	})
}

// =========================================================================
// two generations of legacy headless blobs
// =========================================================================

func TestDecryptLegacyGolden64Mib(t *testing.T) {
	// Fixture derived with the ORIGINAL parameters (released ≤ v1.10.0). The
	// store is configured with unrelated fast params, exercising the chain
	// walk past the store's own set down to the legacy constant.
	store := newTestStore(t)
	seedLegacyCredential(t, store, "old-cred", goldenBlob64)
	cs, err := NewCredentialStoreWithParams(store, goldenPassword, fastTimeCost, fastMemoryCost, fastParallelism)
	require.NoError(t, err)

	pt, err := cs.Retrieve(context.Background(), "old-cred")
	require.NoError(t, err)
	assert.Equal(t, goldenPlaintext, string(pt))
}

func TestDecryptLegacyGolden194Mib(t *testing.T) {
	// Fixture derived with the OWASP-2024 defaults released in v1.11.0
	// (t=3, m=194MiB, p=4) — the generation that the parameter bump
	// orphaned before this fix. Decryption must succeed on a store using
	// the production defaults.
	store := newTestStore(t)
	seedLegacyCredential(t, store, "v111-cred", goldenBlob194)
	cs, err := NewCredentialStore(store, goldenPassword)
	require.NoError(t, err)

	pt, err := cs.Retrieve(context.Background(), "v111-cred")
	require.NoError(t, err)
	assert.Equal(t, goldenPlaintext, string(pt))
}

func TestDecryptLegacyWrongPassword(t *testing.T) {
	store := newTestStore(t)
	seedLegacyCredential(t, store, "old-cred", goldenBlob64)
	cs, err := NewCredentialStoreWithParams(store, "definitely-wrong", fastTimeCost, fastMemoryCost, fastParallelism)
	require.NoError(t, err)

	_, err = cs.Retrieve(context.Background(), "old-cred")
	assert.ErrorIs(t, err, ErrDecryptFailed)
}

func TestRotateMasterPasswordUpgradesLegacyToV1(t *testing.T) {
	// End-to-end upgrade path documented for the v1 rollout: legacy rows
	// decrypt, then RotateMasterPassword re-writes every credential in the
	// self-describing v1 format.
	store := newTestStore(t)
	ctx := context.Background()
	seedLegacyCredential(t, store, "old-a", goldenBlob64)
	seedLegacyCredential(t, store, "old-b", goldenBlob194)

	cs, err := NewCredentialStoreWithParams(store, goldenPassword, fastTimeCost, fastMemoryCost, fastParallelism)
	require.NoError(t, err)

	n, err := cs.RotateMasterPassword(ctx, goldenPassword, "rotated-master")
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	creds, err := cs.List(ctx)
	require.NoError(t, err)
	require.Len(t, creds, 2)
	for _, c := range creds {
		assert.True(t, bytes.HasPrefix(c.EncryptedData, []byte(v1Magic)),
			"credential %q must carry a v1 header after master rotation", c.Name)
	}

	cs2, err := NewCredentialStoreWithParams(store, "rotated-master", fastTimeCost, fastMemoryCost, fastParallelism)
	require.NoError(t, err)
	for _, name := range []string{"old-a", "old-b"} {
		pt, err := cs2.Retrieve(ctx, name)
		require.NoError(t, err)
		assert.Equal(t, goldenPlaintext, string(pt))
	}
}

// =========================================================================
// magic collision and encoding-domain edges
// =========================================================================

func TestMagicCollisionFallsBackToLegacyChain(t *testing.T) {
	// A headless blob whose salt merely begins with "LEV1" (version byte
	// 0xFF ≠ 0x01) must be handled by the legacy chain, not rejected.
	store := newTestStore(t)
	cs, err := NewCredentialStoreWithParams(store, "master-pass", fastTimeCost, fastMemoryCost, fastParallelism)
	require.NoError(t, err)

	salt := append([]byte("LEV1"), bytes.Repeat([]byte{0xFF}, saltLen-4)...)
	nonce := bytes.Repeat([]byte{0x02}, nonceLen)
	key := argon2.Key(cs.masterPassword, salt, fastTimeCost, fastMemoryCost, fastParallelism, keyLen)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	ciphertext := gcm.Seal(nil, nonce, []byte("collision survivor"), nil)
	blob := append(append([]byte{}, salt...), nonce...)
	blob = append(blob, ciphertext...)

	pt, err := cs.decrypt(blob)
	require.NoError(t, err)
	assert.Equal(t, "collision survivor", string(pt))
}

func TestEncryptRejectsUnencodableTimeCost(t *testing.T) {
	// timeCost lives in one header byte; the constructor still accepts the
	// full uint32 domain, so encrypt must police the encoding limit.
	store := newTestStore(t)
	cs, err := NewCredentialStoreWithParams(store, "master-pass", maxV1TimeCost+1, 64*1024, 4)
	require.NoError(t, err)
	_, err = cs.encrypt([]byte("payload"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v1 blob encoding limit")
}

func TestV1HeaderConstantsSelfConsistent(t *testing.T) {
	// Guard the format documentation: header is 39 bytes, minimum blob 55.
	assert.Equal(t, 39, v1HeaderLen)
	assert.Equal(t, 55, minV1BlobLen)
	assert.Equal(t, v1HeaderLen-1, v1OffNonce+int(nonceLen)-1)
	assert.Equal(t, byte('L'), v1Magic[0])
}

// brokenReader always fails, exercising the injectable reader of newID.
type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNewIDFailsOnBrokenReader(t *testing.T) {
	// SA-012: credential ids have no timestamp fallback — a broken random
	// source surfaces as an error (the reader is injectable precisely for
	// this).
	_, err := newID(brokenReader{})
	assert.Error(t, err)
}

func TestLegacyHeadlessBlobsHaveNoMagic(t *testing.T) {
	// Self-check of the fixtures: neither golden blob starts with the v1
	// magic, and neither carries a valid version byte position.
	for _, h := range []string{goldenBlob64, goldenBlob194} {
		b := mustHex(t, h)
		require.False(t, bytes.HasPrefix(b, []byte(v1Magic)))
		require.False(t, strings.Contains(hex.EncodeToString(b[:6]), hex.EncodeToString([]byte(v1Magic))+hex.EncodeToString([]byte{v1Version})))
	}
}
