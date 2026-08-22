// AWS KMS provider for the credential package.
//
// Implements KMSProvider using AWS Key Management Service with envelope
// encryption:
//   - AWS KMS generates a 256-bit data key (GenerateDataKey).
//   - The plaintext data key is used to AES-256-GCM encrypt the credential
//     at rest. The encrypted credential and the KMS-encrypted data key
//     (ciphertext blob) are stored together.
//   - On GetSecret the provider calls KMS Decrypt to recover the plaintext
//     data key, then uses it to AES-256-GCM decrypt the credential.
//   - The plaintext data key is cached in memory with a 1h TTL to amortise
//     KMS API calls; the cache is keyed by the KMS key ARN.
//
// Security:
//   - The plaintext data key lives only in memory; it is never logged and
//     never written to disk. The cache entry is zeroed on eviction.
//   - The plaintext credential returned to the caller is the caller's
//     responsibility; the provider does not retain a copy.
//   - The encrypted data key (KMS ciphertext blob) is stored alongside the
//     encrypted credential; it is opaque to LEVEE and can only be decrypted
//     by KMS.

package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/nexus/levee/internal/log"
)

// --- AWS sentinel errors ----------------------------------------------------

var (
	// ErrAWSMissingKeyID is returned when the KMS key ID is empty.
	ErrAWSMissingKeyID = errors.New("credential: aws kms key id is required")

	// ErrAWSMissingRegion is returned when the AWS region is empty.
	ErrAWSMissingRegion = errors.New("credential: aws region is required")

	// ErrAWSSecretNotFound is returned when the requested secret does not
	// exist in the envelope store.
	ErrAWSSecretNotFound = errors.New("credential: aws kms secret not found")

	// ErrAWSEnvelopeMalformed is returned when the stored envelope is
	// corrupted or has an unexpected version.
	ErrAWSEnvelopeMalformed = errors.New("credential: aws kms envelope malformed")
)

// --- AWSProviderConfig -----------------------------------------------------

// AWSProviderConfig configures the AWS KMS provider.
type AWSProviderConfig struct {
	// KeyID is the KMS key ID or ARN used to generate data keys.
	KeyID string

	// Region is the AWS region (e.g. us-east-1).
	Region string

	// AccessKeyID and SecretAccessKey are static credentials. When both
	// are empty the provider uses the default credential chain (env vars,
	// shared profile, EC2/ECS role).
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string

	// Profile is the named AWS profile to load from the shared config.
	Profile string

	// DataKeyTTL is the in-memory data-key cache TTL. Defaults to 1h.
	DataKeyTTL time.Duration

	// EnvelopeStore is the backing store for encrypted envelopes. When
	// nil the provider uses an in-memory map (useful for tests); in
	// production callers should supply a persistent store (e.g. the same
	// state.Store used by CredentialStore) so envelopes survive restarts.
	EnvelopeStore EnvelopeStore
}

// EnvelopeStore is the persistence interface for encrypted envelopes.
// Implementations may be in-memory (tests), file-backed, or backed by the
// LEVEE state store.
type EnvelopeStore interface {
	// GetEnvelope returns the envelope for the named secret, or
	// (nil, nil) when not found.
	GetEnvelope(ctx context.Context, name string) (*Envelope, error)

	// PutEnvelope stores the envelope for the named secret.
	PutEnvelope(ctx context.Context, name string, env *Envelope) error

	// DeleteEnvelope removes the envelope for the named secret.
	DeleteEnvelope(ctx context.Context, name string) error
}

// Envelope is the on-disk format for an AWS KMS-encrypted credential.
//
// Layout:
//
//	version     — format version (currently 1)
//	keyID       — KMS key ID/ARN that generated the data key
//	edk         — encrypted data key (KMS ciphertext blob, base64)
//	iv          — AES-GCM nonce (base64)
//	ciphertext  — AES-GCM ciphertext (base64)
//	createdAt   — envelope creation time (RFC3339)
type Envelope struct {
	Version    int    `json:"version"`
	KeyID      string `json:"key_id"`
	EDK        string `json:"edk"`
	IV         string `json:"iv"`
	Ciphertext string `json:"ciphertext"`
	CreatedAt  string `json:"created_at"`
}

// --- AWSProvider -----------------------------------------------------------

// kmsAPI is the subset of the AWS KMS client surface used by AWSProvider.
// It is an interface so tests can inject a mock without hitting AWS.
type kmsAPI interface {
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
	GenerateDataKey(ctx context.Context, params *kms.GenerateDataKeyInput, optFns ...func(*kms.Options)) (*kms.GenerateDataKeyOutput, error)
	DescribeKey(ctx context.Context, params *kms.DescribeKeyInput, optFns ...func(*kms.Options)) (*kms.DescribeKeyOutput, error)
}

// AWSProvider implements KMSProvider against AWS KMS using envelope
// encryption. The plaintext data key is cached in memory with a TTL to
// amortise KMS Decrypt calls; the cache is keyed by the KMS key ID.
type AWSProvider struct {
	cfg    AWSProviderConfig
	client kmsAPI

	// dataKeyCache maps keyID → cached data key. Entries expire after
	// DataKeyTTL.
	dataKeyCache sync.Map // map[string]*cachedDataKey

	// store is the envelope store; defaults to an in-memory map.
	store EnvelopeStore

	// mu is reserved for future concurrent credential rotation.
	//nolint:unused // reserved for future use
	mu sync.RWMutex
}

// cachedDataKey holds a plaintext data key and its expiry.
type cachedDataKey struct {
	plaintextKey []byte
	expiresAt    time.Time
}

// NewAWSProvider constructs an AWSProvider. The AWS SDK config is loaded
// from the default chain (env, shared profile, EC2/ECS role) augmented
// with the static credentials in cfg when provided.
func NewAWSProvider(ctx context.Context, cfg AWSProviderConfig) (*AWSProvider, error) {
	if cfg.KeyID == "" {
		return nil, ErrAWSMissingKeyID
	}
	if cfg.Region == "" {
		return nil, ErrAWSMissingRegion
	}
	if cfg.DataKeyTTL == 0 {
		cfg.DataKeyTTL = time.Hour
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
	}
	if cfg.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(cfg.Profile))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("credential: aws config: %w", err)
	}

	client := kms.NewFromConfig(awsCfg)
	p, err := newAWSProviderWithClient(cfg, client)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// newAWSProviderWithClient builds an AWSProvider with a pre-configured KMS
// client. It is extracted from NewAWSProvider so tests can inject a mock
// client without going through the AWS config chain.
func newAWSProviderWithClient(cfg AWSProviderConfig, client kmsAPI) (*AWSProvider, error) {
	if cfg.KeyID == "" {
		return nil, ErrAWSMissingKeyID
	}
	if cfg.DataKeyTTL == 0 {
		cfg.DataKeyTTL = time.Hour
	}
	p := &AWSProvider{
		cfg:    cfg,
		client: client,
	}
	if cfg.EnvelopeStore != nil {
		p.store = cfg.EnvelopeStore
	} else {
		p.store = newMemoryEnvelopeStore()
	}
	return p, nil
}

// Name returns the provider identifier.
func (p *AWSProvider) Name() string { return "aws-kms" }

// --- KMSProvider: GetSecret ------------------------------------------------

// GetSecret retrieves the envelope for name from the store, decrypts the
// data key via KMS Decrypt, and uses the data key to AES-256-GCM decrypt
// the credential. The plaintext data key is cached for DataKeyTTL.
//
// The returned leaseID is empty because AWS KMS does not issue leases;
// ReturnSecret is a no-op for this provider.
func (p *AWSProvider) GetSecret(ctx context.Context, name string) ([]byte, string, error) {
	if name == "" {
		return nil, "", ErrEmptyName
	}

	env, err := p.store.GetEnvelope(ctx, name)
	if err != nil {
		return nil, "", fmt.Errorf("credential: aws kms get envelope %q: %w", name, err)
	}
	if env == nil {
		return nil, "", fmt.Errorf("credential: aws kms secret %q: %w", name, ErrAWSSecretNotFound)
	}

	dataKey, err := p.getOrDecryptDataKey(ctx, env.KeyID, env.EDK)
	if err != nil {
		return nil, "", err
	}
	defer SecureZero(dataKey)

	plaintext, err := decryptEnvelope(dataKey, env)
	if err != nil {
		return nil, "", err
	}

	return plaintext, "", nil
}

// getOrDecryptDataKey returns the cached plaintext data key for the given
// encrypted data key (edk) when the cache entry is fresh; otherwise it
// calls KMS Decrypt to recover the plaintext key and caches it.
//
// The cache is keyed by the encrypted data key (edk) rather than the KMS
// key ID, because each envelope may have its own data key even when they
// share the same KMS master key. Caching by KMS key ID would cause
// decryption failures when multiple envelopes use different data keys
// under the same KMS master key.
func (p *AWSProvider) getOrDecryptDataKey(ctx context.Context, keyID, edk string) ([]byte, error) {
	cacheKey := keyID + ":" + edk
	if cached, ok := p.dataKeyCache.Load(cacheKey); ok {
		c := cached.(*cachedDataKey)
		if time.Now().Before(c.expiresAt) {
			// Return a copy so the caller can zero it without affecting
			// the cached entry.
			out := make([]byte, len(c.plaintextKey))
			copy(out, c.plaintextKey)
			return out, nil
		}
		// Expired; evict.
		SecureZero(c.plaintextKey)
		p.dataKeyCache.Delete(cacheKey)
	}

	edkBytes, err := base64.StdEncoding.DecodeString(edk)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms decode edk: %w", err)
	}

	out, err := p.client.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: edkBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms decrypt data key: %w", err)
	}
	if out == nil || out.Plaintext == nil {
		return nil, fmt.Errorf("credential: aws kms decrypt: nil plaintext in response")
	}

	// Cache the data key. We store a copy so the SDK's buffer can be GC'd.
	dataKey := make([]byte, len(out.Plaintext))
	copy(dataKey, out.Plaintext)
	SecureZero(out.Plaintext)

	p.dataKeyCache.Store(cacheKey, &cachedDataKey{
		plaintextKey: dataKey,
		expiresAt:    time.Now().Add(p.cfg.DataKeyTTL),
	})

	// Return a copy.
	outCopy := make([]byte, len(dataKey))
	copy(outCopy, dataKey)
	return outCopy, nil
}

// decryptEnvelope uses the plaintext data key to AES-256-GCM decrypt the
// envelope's ciphertext.
func decryptEnvelope(dataKey []byte, env *Envelope) ([]byte, error) {
	if env.Version != 1 {
		return nil, fmt.Errorf("credential: aws kms envelope version %d: %w", env.Version, ErrAWSEnvelopeMalformed)
	}
	if len(dataKey) != 32 {
		return nil, fmt.Errorf("credential: aws kms data key length %d: %w", len(dataKey), ErrAWSEnvelopeMalformed)
	}

	block, err := aes.NewCipher(dataKey)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms new gcm: %w", err)
	}

	iv, err := base64.StdEncoding.DecodeString(env.IV)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms decode iv: %w", err)
	}
	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("credential: aws kms iv length %d: %w", len(iv), ErrAWSEnvelopeMalformed)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms decode ciphertext: %w", err)
	}

	plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("credential: aws kms gcm open: %w", err)
	}
	return plaintext, nil
}

// --- KMSProvider: ReturnSecret ---------------------------------------------

// ReturnSecret is a no-op for the AWS KMS provider because AWS KMS does
// not issue leases. The plaintext data key remains cached until its TTL
// expires; callers who want to evict it immediately can call
// FlushDataKeyCache.
func (p *AWSProvider) ReturnSecret(ctx context.Context, leaseID string) error {
	return nil
}

// --- KMSProvider: HealthCheck ----------------------------------------------

// HealthCheck verifies that the KMS key is reachable by calling
// DescribeKey on the configured KeyID.
func (p *AWSProvider) HealthCheck(ctx context.Context) error {
	_, err := p.client.DescribeKey(ctx, &kms.DescribeKeyInput{
		KeyId: &p.cfg.KeyID,
	})
	if err != nil {
		return fmt.Errorf("credential: aws kms describe key: %w", err)
	}
	return nil
}

// --- Envelope management (PutSecret, DeleteSecret) -------------------------

// PutSecret encrypts plaintext using a fresh data key from KMS and stores
// the resulting envelope under name. This is the write-side companion to
// GetSecret; it is not part of the KMSProvider interface but is exposed so
// callers can populate the envelope store out-of-band (e.g. via a CLI
// command).
func (p *AWSProvider) PutSecret(ctx context.Context, name string, plaintext []byte) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(plaintext) == 0 {
		return ErrEmptyPlaintext
	}

	// Generate a fresh data key. We use GenerateDataKey so KMS returns both
	// the plaintext key (for local encryption) and the encrypted key (for
	// storage).
	out, err := p.client.GenerateDataKey(ctx, &kms.GenerateDataKeyInput{
		KeyId:   &p.cfg.KeyID,
		KeySpec: types.DataKeySpecAes256,
	})
	if err != nil {
		return fmt.Errorf("credential: aws kms generate data key: %w", err)
	}
	if out == nil || out.Plaintext == nil || out.CiphertextBlob == nil {
		return fmt.Errorf("credential: aws kms generate data key: nil response")
	}
	defer SecureZero(out.Plaintext)

	if len(out.Plaintext) != 32 {
		return fmt.Errorf("credential: aws kms data key length %d: %w", len(out.Plaintext), ErrAWSEnvelopeMalformed)
	}

	block, err := aes.NewCipher(out.Plaintext)
	if err != nil {
		return fmt.Errorf("credential: aws kms new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("credential: aws kms new gcm: %w", err)
	}

	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return fmt.Errorf("credential: aws kms generate iv: %w", err)
	}

	ciphertext := gcm.Seal(nil, iv, plaintext, nil)

	env := &Envelope{
		Version:    1,
		KeyID:      p.cfg.KeyID,
		EDK:        base64.StdEncoding.EncodeToString(out.CiphertextBlob),
		IV:         base64.StdEncoding.EncodeToString(iv),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	if err := p.store.PutEnvelope(ctx, name, env); err != nil {
		return fmt.Errorf("credential: aws kms put envelope %q: %w", name, err)
	}
	log.DebugCtx(ctx, "aws kms secret stored", "name", name, "key_id", p.cfg.KeyID)
	return nil
}

// DeleteSecret removes the envelope for name from the store.
func (p *AWSProvider) DeleteSecret(ctx context.Context, name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if err := p.store.DeleteEnvelope(ctx, name); err != nil {
		return fmt.Errorf("credential: aws kms delete envelope %q: %w", name, err)
	}
	return nil
}

// --- Cache management ------------------------------------------------------

// FlushDataKeyCache evicts all cached data keys, zeroing them in memory.
// Call this when you suspect key compromise or want to force fresh KMS
// Decrypt calls.
func (p *AWSProvider) FlushDataKeyCache() {
	p.dataKeyCache.Range(func(key, value any) bool {
		c, _ := value.(*cachedDataKey)
		if c != nil {
			SecureZero(c.plaintextKey)
		}
		p.dataKeyCache.Delete(key)
		return true
	})
}

// --- In-memory envelope store (default for tests) --------------------------

// memoryEnvelopeStore is the default EnvelopeStore when none is provided.
type memoryEnvelopeStore struct {
	mu   sync.RWMutex
	data map[string]*Envelope
}

func newMemoryEnvelopeStore() *memoryEnvelopeStore {
	return &memoryEnvelopeStore{data: make(map[string]*Envelope)}
}

func (s *memoryEnvelopeStore) GetEnvelope(_ context.Context, name string) (*Envelope, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	env, ok := s.data[name]
	if !ok {
		return nil, nil
	}
	// Return a copy so callers cannot mutate the stored envelope.
	cp := *env
	return &cp, nil
}

func (s *memoryEnvelopeStore) PutEnvelope(_ context.Context, name string, env *Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *env
	s.data[name] = &cp
	return nil
}

func (s *memoryEnvelopeStore) DeleteEnvelope(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, name)
	return nil
}

// --- Compile-time interface check ------------------------------------------

var _ EnvelopeStore = (*memoryEnvelopeStore)(nil)

// ensure strings is used so the import is not dropped when we trim unused
// symbols in a future refactor.
var _ = strings.HasPrefix
