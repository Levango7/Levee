package credential

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newFastAlgorithmStore builds a store with cheap argon2 params (same
// package access) and the requested algorithm name.
func newFastAlgorithmStore(t *testing.T, master, algorithm string) *CredentialStore {
	t.Helper()
	cs, err := NewCredentialStoreWithAlgorithm(newTestStore(t), master, algorithm)
	require.NoError(t, err)
	cs.timeCost = fastTimeCost
	cs.memoryCost = fastMemoryCost
	cs.parallelism = fastParallelism
	return cs
}

func TestSM4GCMRoundTrip(t *testing.T) {
	cs := newFastAlgorithmStore(t, "gm-master", encryptionSM4GCM)
	plaintext := []byte("国密 SM4-GCM 凭据载荷")
	blob, err := cs.encrypt(plaintext)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(blob, []byte(v2Magic)), "SM4 mode must write a v2 blob")
	assert.Equal(t, algSM4GCM, blob[v2OffAlgo])
	assert.Len(t, blob, v2HeaderLen+len(plaintext)+v1TagLen)

	got, err := cs.decrypt(blob)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
}

func TestV1AndV2CrossRead(t *testing.T) {
	aesCS := newFastAlgorithmStore(t, "shared-master", encryptionAES256GCM)
	sm4CS := newFastAlgorithmStore(t, "shared-master", encryptionSM4GCM)

	aesBlob, err := aesCS.encrypt([]byte("aes payload"))
	require.NoError(t, err)
	sm4Blob, err := sm4CS.encrypt([]byte("sm4 payload"))
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(aesBlob, []byte(v1Magic)))
	assert.True(t, bytes.HasPrefix(sm4Blob, []byte(v2Magic)))

	// Both readers can decrypt both formats — algorithm choice is a writer
	// property and never orphans existing credentials.
	for name, cs := range map[string]*CredentialStore{"aes-reader": aesCS, "sm4-reader": sm4CS} {
		got, err := cs.decrypt(aesBlob)
		require.NoError(t, err, name)
		assert.Equal(t, []byte("aes payload"), got, name)
		got, err = cs.decrypt(sm4Blob)
		require.NoError(t, err, name)
		assert.Equal(t, []byte("sm4 payload"), got, name)
	}
}

func TestUnknownAlgorithmRejected(t *testing.T) {
	_, err := NewCredentialStoreWithAlgorithm(newTestStore(t), "master", "des-gcm")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported encryption algorithm")
}

func TestV2UnknownAlgorithmTagHardFails(t *testing.T) {
	cs := newFastAlgorithmStore(t, "gm-master", encryptionSM4GCM)
	blob, err := cs.encrypt([]byte("payload"))
	require.NoError(t, err)
	corrupt := append([]byte(nil), blob...)
	corrupt[v2OffAlgo] = 0xff
	_, err = cs.decrypt(corrupt)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown algorithm tag")
}
