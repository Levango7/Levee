package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mockKMSAPI ------------------------------------------------------------

// mockKMSAPI implements kmsAPI for tests.
type mockKMSAPI struct {
	// decryptFn is called by Decrypt. When nil, Decrypt returns the
	// pre-configured plaintext for the ciphertext blob.
	decryptFn func(ctx context.Context, params *kms.DecryptInput) (*kms.DecryptOutput, error)

	// generateDataKeyFn is called by GenerateDataKey.
	generateDataKeyFn func(ctx context.Context, params *kms.GenerateDataKeyInput) (*kms.GenerateDataKeyOutput, error)

	// describeKeyFn is called by DescribeKey.
	describeKeyFn func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error)

	// dataKeys maps ciphertext blob → plaintext key.
	dataKeys map[string][]byte

	// decryptCount counts Decrypt calls.
	decryptCount int

	// generateCount counts GenerateDataKey calls.
	generateCount int
}

func newMockKMSAPI() *mockKMSAPI {
	return &mockKMSAPI{dataKeys: make(map[string][]byte)}
}

func (m *mockKMSAPI) Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	m.decryptCount++
	if m.decryptFn != nil {
		return m.decryptFn(ctx, params)
	}
	// The mock GenerateDataKey uses the base64-encoded plaintext as the
	// ciphertext blob, so we look up by the raw blob bytes.
	key := string(params.CiphertextBlob)
	pt, ok := m.dataKeys[key]
	if !ok {
		return nil, errors.New("mock: unknown ciphertext blob")
	}
	ptCopy := make([]byte, len(pt))
	copy(ptCopy, pt)
	return &kms.DecryptOutput{Plaintext: ptCopy}, nil
}

func (m *mockKMSAPI) GenerateDataKey(ctx context.Context, params *kms.GenerateDataKeyInput, optFns ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error) {
	m.generateCount++
	if m.generateDataKeyFn != nil {
		return m.generateDataKeyFn(ctx, params)
	}
	// Generate a random 32-byte data key.
	plaintext := make([]byte, 32)
	if _, err := rand.Read(plaintext); err != nil {
		return nil, err
	}
	// The "ciphertext blob" is just the plaintext base64-encoded so the
	// mock Decrypt can reverse it. In real KMS this would be encrypted.
	ciphertextBlob := []byte(base64.StdEncoding.EncodeToString(plaintext))
	m.dataKeys[string(ciphertextBlob)] = plaintext

	ptCopy := make([]byte, len(plaintext))
	copy(ptCopy, plaintext)
	return &kms.GenerateDataKeyOutput{
		Plaintext:      ptCopy,
		CiphertextBlob: ciphertextBlob,
	}, nil
}

func (m *mockKMSAPI) DescribeKey(ctx context.Context, params *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error) {
	if m.describeKeyFn != nil {
		return m.describeKeyFn(ctx, params)
	}
	return &kms.DescribeKeyOutput{}, nil
}

// --- newTestAWSProvider ----------------------------------------------------

// newTestAWSProvider builds an AWSProvider backed by a mock KMS API and an
// in-memory envelope store.
func newTestAWSProvider(t *testing.T) (*AWSProvider, *mockKMSAPI) {
	t.Helper()
	mock := newMockKMSAPI()
	p, err := newAWSProviderWithClient(AWSProviderConfig{
		KeyID:      "arn:aws:kms:us-east-1:123:key/test",
		Region:     "us-east-1",
		DataKeyTTL: 100 * time.Millisecond, // short TTL for cache tests
	}, mock)
	require.NoError(t, err)
	return p, mock
}

// =========================================================================
// NewAWSProvider / newAWSProviderWithClient
// =========================================================================

func TestNewAWSProvider(t *testing.T) {
	t.Run("missing key id", func(t *testing.T) {
		_, err := newAWSProviderWithClient(AWSProviderConfig{
			Region: "us-east-1",
		}, newMockKMSAPI())
		assert.ErrorIs(t, err, ErrAWSMissingKeyID)
	})

	t.Run("ok with mock client", func(t *testing.T) {
		p, err := newAWSProviderWithClient(AWSProviderConfig{
			KeyID:  "key-1",
			Region: "us-east-1",
		}, newMockKMSAPI())
		require.NoError(t, err)
		assert.NotNil(t, p)
		assert.Equal(t, "aws-kms", p.Name())
	})
}

// =========================================================================
// PutSecret + GetSecret round-trip
// =========================================================================

func TestAWSProvider_PutGetSecret_RoundTrip(t *testing.T) {
	p, _ := newTestAWSProvider(t)
	ctx := context.Background()

	// Store a secret.
	plaintext := []byte("super-secret-value")
	require.NoError(t, p.PutSecret(ctx, "db-prod", plaintext))

	// Retrieve it.
	got, leaseID, err := p.GetSecret(ctx, "db-prod")
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)
	// AWS KMS does not issue leases.
	assert.Empty(t, leaseID)
}

func TestAWSProvider_PutSecret(t *testing.T) {
	p, _ := newTestAWSProvider(t)
	ctx := context.Background()

	t.Run("empty name", func(t *testing.T) {
		err := p.PutSecret(ctx, "", []byte("x"))
		assert.ErrorIs(t, err, ErrEmptyName)
	})

	t.Run("empty plaintext", func(t *testing.T) {
		err := p.PutSecret(ctx, "name", nil)
		assert.ErrorIs(t, err, ErrEmptyPlaintext)
	})

	t.Run("generate data key error", func(t *testing.T) {
		mock := newMockKMSAPI()
		mock.generateDataKeyFn = func(ctx context.Context, params *kms.GenerateDataKeyInput) (*kms.GenerateDataKeyOutput, error) {
			return nil, errors.New("kms error")
		}
		p, err := newAWSProviderWithClient(AWSProviderConfig{
			KeyID:  "key-1",
			Region: "us-east-1",
		}, mock)
		require.NoError(t, err)

		err = p.PutSecret(ctx, "name", []byte("value"))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "kms error")
	})
}

func TestAWSProvider_GetSecret(t *testing.T) {
	ctx := context.Background()

	t.Run("empty name", func(t *testing.T) {
		p, _ := newTestAWSProvider(t)
		_, _, err := p.GetSecret(ctx, "")
		assert.ErrorIs(t, err, ErrEmptyName)
	})

	t.Run("not found", func(t *testing.T) {
		p, _ := newTestAWSProvider(t)
		_, _, err := p.GetSecret(ctx, "missing")
		assert.Error(t, err)
	})

	t.Run("decrypt error", func(t *testing.T) {
		mock := newMockKMSAPI()
		mock.decryptFn = func(ctx context.Context, params *kms.DecryptInput) (*kms.DecryptOutput, error) {
			return nil, errors.New("decrypt failed")
		}
		p, err := newAWSProviderWithClient(AWSProviderConfig{
			KeyID:  "key-1",
			Region: "us-east-1",
		}, mock)
		require.NoError(t, err)

		// Manually create an envelope that will trigger decrypt.
		env := &Envelope{
			Version:    1,
			KeyID:      "key-1",
			EDK:        base64.StdEncoding.EncodeToString([]byte("fake-edk")),
			IV:         base64.StdEncoding.EncodeToString(make([]byte, 12)),
			Ciphertext: base64.StdEncoding.EncodeToString([]byte("fake-ct")),
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		}
		require.NoError(t, p.store.PutEnvelope(ctx, "test", env))

		_, _, err = p.GetSecret(ctx, "test")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "decrypt failed")
	})
}

// =========================================================================
// Data key cache
// =========================================================================

func TestAWSProvider_DataKeyCache(t *testing.T) {
	p, mock := newTestAWSProvider(t)
	ctx := context.Background()

	// Store a secret.
	require.NoError(t, p.PutSecret(ctx, "a", []byte("val-a")))
	require.Equal(t, 1, mock.generateCount)

	// First GetSecret: should call KMS Decrypt to recover the data key.
	_, _, err := p.GetSecret(ctx, "a")
	require.NoError(t, err)
	firstDecryptCount := mock.decryptCount
	assert.GreaterOrEqual(t, firstDecryptCount, 1)

	// Second GetSecret for the same secret: the data key is cached, so
	// no additional Decrypt call should be made.
	_, _, err = p.GetSecret(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, firstDecryptCount, mock.decryptCount)
}

func TestAWSProvider_FlushDataKeyCache(t *testing.T) {
	p, mock := newTestAWSProvider(t)
	ctx := context.Background()

	require.NoError(t, p.PutSecret(ctx, "a", []byte("val-a")))
	_, _, err := p.GetSecret(ctx, "a")
	require.NoError(t, err)
	countBeforeFlush := mock.decryptCount

	// Flush the cache.
	p.FlushDataKeyCache()

	// Next GetSecret should call Decrypt again.
	_, _, err = p.GetSecret(ctx, "a")
	require.NoError(t, err)
	assert.Greater(t, mock.decryptCount, countBeforeFlush)
}

// =========================================================================
// ReturnSecret
// =========================================================================

func TestAWSProvider_ReturnSecret(t *testing.T) {
	p, _ := newTestAWSProvider(t)
	// ReturnSecret is a no-op for AWS KMS.
	err := p.ReturnSecret(context.Background(), "any-lease")
	assert.NoError(t, err)
}

// =========================================================================
// HealthCheck
// =========================================================================

func TestAWSProvider_HealthCheck(t *testing.T) {
	t.Run("healthy", func(t *testing.T) {
		p, _ := newTestAWSProvider(t)
		err := p.HealthCheck(context.Background())
		assert.NoError(t, err)
	})

	t.Run("error", func(t *testing.T) {
		mock := newMockKMSAPI()
		mock.describeKeyFn = func(ctx context.Context, params *kms.DescribeKeyInput) (*kms.DescribeKeyOutput, error) {
			return nil, errors.New("kms unreachable")
		}
		p, err := newAWSProviderWithClient(AWSProviderConfig{
			KeyID:  "key-1",
			Region: "us-east-1",
		}, mock)
		require.NoError(t, err)

		err = p.HealthCheck(context.Background())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "kms unreachable")
	})
}

// =========================================================================
// DeleteSecret
// =========================================================================

func TestAWSProvider_DeleteSecret(t *testing.T) {
	p, _ := newTestAWSProvider(t)
	ctx := context.Background()

	require.NoError(t, p.PutSecret(ctx, "a", []byte("val")))

	err := p.DeleteSecret(ctx, "a")
	assert.NoError(t, err)

	// After delete, GetSecret should fail.
	_, _, err = p.GetSecret(ctx, "a")
	assert.Error(t, err)
}

// =========================================================================
// memoryEnvelopeStore
// =========================================================================

func TestMemoryEnvelopeStore(t *testing.T) {
	s := newMemoryEnvelopeStore()
	ctx := context.Background()

	t.Run("get nonexistent returns nil", func(t *testing.T) {
		env, err := s.GetEnvelope(ctx, "missing")
		require.NoError(t, err)
		assert.Nil(t, env)
	})

	t.Run("put and get", func(t *testing.T) {
		env := &Envelope{Version: 1, KeyID: "k"}
		require.NoError(t, s.PutEnvelope(ctx, "a", env))

		got, err := s.GetEnvelope(ctx, "a")
		require.NoError(t, err)
		assert.Equal(t, 1, got.Version)
		assert.Equal(t, "k", got.KeyID)
	})

	t.Run("delete", func(t *testing.T) {
		require.NoError(t, s.PutEnvelope(ctx, "b", &Envelope{Version: 1}))
		require.NoError(t, s.DeleteEnvelope(ctx, "b"))
		got, err := s.GetEnvelope(ctx, "b")
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("put returns copy", func(t *testing.T) {
		env := &Envelope{Version: 1, KeyID: "original"}
		require.NoError(t, s.PutEnvelope(ctx, "c", env))

		// Mutate the original; the stored copy should be unaffected.
		env.KeyID = "mutated"
		got, err := s.GetEnvelope(ctx, "c")
		require.NoError(t, err)
		assert.Equal(t, "original", got.KeyID)
	})
}

// =========================================================================
// Envelope encryption helpers (decryptEnvelope)
// =========================================================================

func TestDecryptEnvelope(t *testing.T) {
	t.Run("wrong version", func(t *testing.T) {
		env := &Envelope{Version: 99}
		_, err := decryptEnvelope(make([]byte, 32), env)
		assert.ErrorIs(t, err, ErrAWSEnvelopeMalformed)
	})

	t.Run("wrong key length", func(t *testing.T) {
		env := &Envelope{Version: 1}
		_, err := decryptEnvelope(make([]byte, 16), env)
		assert.Error(t, err)
	})

	t.Run("round-trip", func(t *testing.T) {
		// Generate a data key.
		dataKey := make([]byte, 32)
		_, err := rand.Read(dataKey)
		require.NoError(t, err)

		// Encrypt a plaintext.
		block, err := aes.NewCipher(dataKey)
		require.NoError(t, err)
		gcm, err := cipher.NewGCM(block)
		require.NoError(t, err)

		iv := make([]byte, gcm.NonceSize())
		_, err = rand.Read(iv)
		require.NoError(t, err)

		plaintext := []byte("hello world")
		ct := gcm.Seal(nil, iv, plaintext, nil)

		env := &Envelope{
			Version:    1,
			IV:         base64.StdEncoding.EncodeToString(iv),
			Ciphertext: base64.StdEncoding.EncodeToString(ct),
		}

		got, err := decryptEnvelope(dataKey, env)
		require.NoError(t, err)
		assert.Equal(t, plaintext, got)
	})
}

// ensure types.DataKeySpecAes256 is referenced so the import is not dropped.
var _ = types.DataKeySpecAes256
