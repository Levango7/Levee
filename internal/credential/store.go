// Package credential implements encrypted at-rest storage for sensitive
// credentials (SSH keys, passwords, API tokens) used by LEVEE's executor
// subsystem.
//
// Security model:
//   - AES-256-GCM provides authenticated encryption of credential plaintext.
//   - argon2id derives a per-credential 32-byte key from a master password
//     and a per-credential random salt, so compromising one ciphertext does
//     not reveal the master password or other credentials.
//   - The master password lives only in process memory; it is never written
//     to disk, never logged, and never emitted to the audit trace.
//   - Credential plaintext is zeroed immediately after encryption.
//
// This package does NOT call audit.TraceRecorder and does NOT log plaintext.
// Logs include only the credential name and the operation result.
package credential

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// --- Constants --------------------------------------------------------------

// Key length for AES-256 (32 bytes).
const keyLen = 32

// Salt length for argon2id (16 bytes).
const saltLen = 16

// Nonce length for AES-GCM (12 bytes, gcm.NonceSize()).
const nonceLen = 12

// Default argon2id parameters. These match the OWASP 2024 recommended
// minimums (time=3, memory=194MiB=197888KiB, parallelism=4). The memory
// cost was raised from 64MiB to 194MiB to comply with OWASP Password
// Storage Cheat Sheet (2024) which recommends m ≥ 194 MiB for t=3, p=4.
// Callers may override these via NewCredentialStoreWithParams when weaker
// parameters are acceptable for non-production or test scenarios.
const (
	defaultTimeCost    uint32 = 3
	defaultMemoryCost  uint32 = 194 * 1024
	defaultParallelism uint8  = 4
)

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrEmptyMasterPassword is returned when the master password is empty.
	ErrEmptyMasterPassword = errors.New("credential: empty master password")

	// ErrEmptyName is returned when the credential name is empty.
	ErrEmptyName = errors.New("credential: empty credential name")

	// ErrEmptyPlaintext is returned when the credential plaintext is empty.
	ErrEmptyPlaintext = errors.New("credential: empty plaintext")

	// ErrNotFound is returned when the credential does not exist.
	ErrNotFound = errors.New("credential: not found")

	// ErrDecryptFailed is returned when decryption fails (e.g. wrong master
	// password or corrupted ciphertext).
	ErrDecryptFailed = errors.New("credential: decryption failed")

	// ErrInvalidCiphertext is returned when the stored ciphertext is too
	// short or malformed.
	ErrInvalidCiphertext = errors.New("credential: invalid ciphertext format")

	// ErrMasterPasswordMismatch is returned by RotateMasterPassword when
	// the provided old password does not match the current master password.
	ErrMasterPasswordMismatch = errors.New("credential: old master password does not match")
)

// --- CredentialStore --------------------------------------------------------

// CredentialStore manages encrypted storage and decrypted retrieval of
// credentials. It uses AES-256-GCM for authenticated encryption and
// argon2id to derive a per-credential key from the master password and a
// per-credential random salt. The master password lives only in memory;
// it is never persisted, logged, or emitted to the audit trace.
type CredentialStore struct {
	store          state.Store
	masterPassword []byte // 仅内存；不序列化、不持久化、不进日志、不进 trace
	timeCost       uint32
	memoryCost     uint32
	parallelism    uint8
}

// CredentialSpec is the input for creating or updating a credential.
// The Plaintext field is zeroed immediately after encryption, so callers
// should not retain references to it.
type CredentialSpec struct {
	Name      string            // 凭据名（唯一键）
	Type      string            // 凭据类型：ssh_key, ssh_password, winrm_password, api_token, ...
	Plaintext []byte            // 凭据明文（加密后清零）
	Tags      map[string]string // 标签（如 target=host-a, env=prod）；当前未持久化，保留以供未来扩展
}

// NewCredentialStore creates a new CredentialStore backed by the given
// state.Store. The masterPassword is used to derive per-credential
// encryption keys via argon2id; it is kept only in process memory.
//
// masterPassword must be non-empty; otherwise ErrEmptyMasterPassword is
// returned. store must be non-nil; otherwise an error is returned.
//
// Default argon2id parameters are used (time=3, memory=194MiB, parallelism=4),
// matching the OWASP 2024 recommended minimums. Use NewCredentialStoreWithParams
// to override these for tests or constrained environments.
func NewCredentialStore(store state.Store, masterPassword string) (*CredentialStore, error) {
	if store == nil {
		return nil, fmt.Errorf("credential: nil store")
	}
	if masterPassword == "" {
		return nil, ErrEmptyMasterPassword
	}

	// Copy the master password into a private byte slice so the caller's
	// string cannot be aliased into our memory. The string itself is
	// immutable in Go, but we want a stable in-memory copy we control.
	mp := make([]byte, len(masterPassword))
	copy(mp, masterPassword)

	return &CredentialStore{
		store:          store,
		masterPassword: mp,
		timeCost:       defaultTimeCost,
		memoryCost:     defaultMemoryCost,
		parallelism:    defaultParallelism,
	}, nil
}

// NewCredentialStoreWithParams is like NewCredentialStore but allows the
// caller to override the argon2id cost parameters. This is intended for
// test scenarios or constrained environments where the OWASP 2024 defaults
// (time=3, memory=194MiB, parallelism=4) are too expensive.
//
// Validation:
//   - timeCost must be >= 1.
//   - memoryCost is in KiB and must be >= 8 * parallelism (argon2 requirement).
//   - parallelism must be >= 1 and <= 255.
//
// Passing zero values for all three falls back to the package defaults.
func NewCredentialStoreWithParams(store state.Store, masterPassword string, timeCost uint32, memoryCost uint32, parallelism uint8) (*CredentialStore, error) {
	if store == nil {
		return nil, fmt.Errorf("credential: nil store")
	}
	if masterPassword == "" {
		return nil, ErrEmptyMasterPassword
	}

	// Fall back to defaults when caller passes all-zero values.
	if timeCost == 0 && memoryCost == 0 && parallelism == 0 {
		timeCost = defaultTimeCost
		memoryCost = defaultMemoryCost
		parallelism = defaultParallelism
	}

	// Validate overrides.
	if timeCost == 0 {
		return nil, fmt.Errorf("credential: invalid timeCost: must be >= 1")
	}
	if parallelism == 0 {
		return nil, fmt.Errorf("credential: invalid parallelism: must be >= 1")
	}
	// argon2.Key panics when memoryCost < 8*parallelism; guard against that
	// rather than letting a panic escape from this package.
	if memoryCost < uint32(parallelism)*8 {
		return nil, fmt.Errorf("credential: invalid memoryCost: must be >= 8*parallelism (%d)", uint32(parallelism)*8)
	}

	mp := make([]byte, len(masterPassword))
	copy(mp, masterPassword)

	return &CredentialStore{
		store:          store,
		masterPassword: mp,
		timeCost:       timeCost,
		memoryCost:     memoryCost,
		parallelism:    parallelism,
	}, nil
}

// --- Encryption helpers -----------------------------------------------------

// deriveKey derives a 32-byte AES-256 key from the master password and
// the given salt using argon2id. The returned key must be zeroed by the
// caller when no longer needed.
func (cs *CredentialStore) deriveKey(salt []byte) []byte {
	return argon2.Key(cs.masterPassword, salt, cs.timeCost, cs.memoryCost, cs.parallelism, keyLen)
}

// encrypt encrypts plaintext using AES-256-GCM with a per-credential key
// derived from the master password and a fresh random salt. The returned
// blob has the format: salt(16) || nonce(12) || ciphertext.
func (cs *CredentialStore) encrypt(plaintext []byte) ([]byte, error) {
	salt := make([]byte, saltLen)
	if n, err := rand.Read(salt); err != nil || n != saltLen {
		return nil, fmt.Errorf("credential: generate salt: %w", err)
	}

	key := cs.deriveKey(salt)
	defer SecureZero(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: new gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if n, err := rand.Read(nonce); err != nil || n != gcm.NonceSize() {
		return nil, fmt.Errorf("credential: generate nonce: %w", err)
	}

	// Seal appends ciphertext+tag to the dst (nil) and returns the whole
	// thing. We assemble salt || nonce || ciphertext manually.
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	blob := make([]byte, 0, saltLen+len(nonce)+len(ciphertext))
	blob = append(blob, salt...)
	blob = append(blob, nonce...)
	blob = append(blob, ciphertext...)
	return blob, nil
}

// decrypt decrypts a blob produced by encrypt back to the original plaintext.
// The blob format is: salt(16) || nonce(12) || ciphertext.
func (cs *CredentialStore) decrypt(blob []byte) ([]byte, error) {
	if len(blob) < saltLen+nonceLen {
		return nil, ErrInvalidCiphertext
	}
	salt := blob[:saltLen]
	nonce := blob[saltLen : saltLen+nonceLen]
	ciphertext := blob[saltLen+nonceLen:]

	key := cs.deriveKey(salt)
	defer SecureZero(key)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("credential: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential: new gcm: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecryptFailed, err)
	}
	return plaintext, nil
}

// --- SecureZero -------------------------------------------------------------

// SecureZero overwrites the given byte slice with zeros. Use it to clear
// sensitive material (plaintext, derived keys) from memory as soon as it
// is no longer needed.
//
// Note: Go's escape analysis and GC may copy slice data, so this is a
// best-effort wipe. It is still strictly better than leaving plaintext
// in memory indefinitely.
//
// The trailing runtime.KeepAlive(b) prevents the compiler from optimizing
// the zeroing loop away when the slice is not read afterwards (a known
// hazard for dead-store elimination on sensitive buffers). Callers do not
// need to add their own KeepAlive after calling SecureZero.
func SecureZero(b []byte) {
	for i := range b {
		b[i] = 0
	}
	// KeepAlive pins the slice header so the compiler cannot prove the
	// write is unobservable and elide it. This is the standard Go idiom
	// for securing a wipe against dead-store optimization.
	runtime.KeepAlive(b)
}

// --- Store / Retrieve / Delete / List / Rotate ------------------------------

// Store encrypts the credential plaintext and persists it. The plaintext
// is zeroed immediately after encryption, regardless of success or failure.
// The returned state.Credential does not contain the plaintext.
//
// name and plaintext must be non-empty; otherwise ErrEmptyName or
// ErrEmptyPlaintext is returned.
func (cs *CredentialStore) Store(ctx context.Context, spec CredentialSpec) (*state.Credential, error) {
	// Always zero the plaintext on return, regardless of the outcome.
	// spec is a value receiver, but spec.Plaintext shares its backing
	// array with the caller's slice, so this zeroes the caller's bytes too.
	defer SecureZero(spec.Plaintext)

	if spec.Name == "" {
		return nil, ErrEmptyName
	}
	if len(spec.Plaintext) == 0 {
		return nil, ErrEmptyPlaintext
	}

	blob, err := cs.encrypt(spec.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("credential: encrypt: %w", err)
	}

	now := time.Now().UTC()
	cred := &state.Credential{
		ID:            newID(),
		Name:          spec.Name,
		Type:          spec.Type,
		EncryptedData: blob,
		CreatedAt:     now,
	}
	if err := cs.store.CreateCredential(ctx, cred); err != nil {
		return nil, fmt.Errorf("credential: persist: %w", err)
	}

	// Log only the credential name and type; never the plaintext or ciphertext.
	log.InfoCtx(ctx, "credential stored",
		"name", spec.Name,
		"type", spec.Type,
	)
	return cred, nil
}

// Retrieve reads and decrypts the credential with the given name. The
// returned plaintext is the caller's responsibility; use SecureZero on
// it as soon as it is no longer needed.
//
// Returns ErrNotFound when the credential does not exist and
// ErrDecryptFailed when the master password does not match the one used
// at encryption time.
func (cs *CredentialStore) Retrieve(ctx context.Context, name string) ([]byte, error) {
	if name == "" {
		return nil, ErrEmptyName
	}

	cred, err := cs.store.GetCredentialByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("credential: load: %w", err)
	}
	if cred == nil {
		return nil, ErrNotFound
	}

	plaintext, err := cs.decrypt(cred.EncryptedData)
	if err != nil {
		return nil, err
	}

	// Debug-level only, and only the name — never the plaintext.
	log.DebugCtx(ctx, "credential retrieved", "name", name)
	return plaintext, nil
}

// Delete removes the credential with the given name. Returns ErrNotFound
// when the credential does not exist.
func (cs *CredentialStore) Delete(ctx context.Context, name string) error {
	if name == "" {
		return ErrEmptyName
	}

	cred, err := cs.store.GetCredentialByName(ctx, name)
	if err != nil {
		return fmt.Errorf("credential: load: %w", err)
	}
	if cred == nil {
		return ErrNotFound
	}

	if err := cs.store.DeleteCredential(ctx, cred.ID); err != nil {
		return fmt.Errorf("credential: delete: %w", err)
	}

	log.InfoCtx(ctx, "credential deleted", "name", name)
	return nil
}

// List returns metadata for all stored credentials, ordered by name.
// The returned state.Credential entries contain the ciphertext blob in
// EncryptedData but never the plaintext.
func (cs *CredentialStore) List(ctx context.Context) ([]*state.Credential, error) {
	creds, err := cs.store.ListCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("credential: list: %w", err)
	}
	return creds, nil
}

// Rotate replaces the encrypted plaintext of an existing credential with
// newPlaintext. The newPlaintext is zeroed immediately after encryption,
// regardless of success or failure. The credential's RotatedAt timestamp
// is updated to the current time.
//
// Returns ErrNotFound when the credential does not exist.
func (cs *CredentialStore) Rotate(ctx context.Context, name string, newPlaintext []byte) (*state.Credential, error) {
	// Always zero the new plaintext on return.
	defer SecureZero(newPlaintext)

	if name == "" {
		return nil, ErrEmptyName
	}
	if len(newPlaintext) == 0 {
		return nil, ErrEmptyPlaintext
	}

	cred, err := cs.store.GetCredentialByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("credential: load: %w", err)
	}
	if cred == nil {
		return nil, ErrNotFound
	}

	blob, err := cs.encrypt(newPlaintext)
	if err != nil {
		return nil, fmt.Errorf("credential: encrypt: %w", err)
	}

	now := time.Now().UTC()
	cred.EncryptedData = blob
	cred.RotatedAt = &now
	if err := cs.store.UpdateCredential(ctx, cred); err != nil {
		return nil, fmt.Errorf("credential: persist: %w", err)
	}

	log.InfoCtx(ctx, "credential rotated", "name", name)
	return cred, nil
}

// rotationUndo captures everything needed to undo one persisted step of a
// master-password rotation: the affected row and its pre-rotation ciphertext.
type rotationUndo struct {
	cred    *state.Credential
	oldBlob []byte
}

// RotateMasterPassword re-encrypts all stored credentials with a new master
// password. The oldPW must match the current master password; otherwise
// ErrMasterPasswordMismatch is returned.
//
// Failure atomicity: every new ciphertext is computed up front; each row is
// then persisted in order. If persisting item k fails, items 0..k-1 are
// rolled back by re-persisting their OLD ciphertext blobs, leaving the store
// exactly as before the call (the in-memory master password is not switched).
// If the rollback itself fails for some rows, a fatal error enumerates the
// unrecoverable credential ids: those rows hold new-password ciphertext while
// the active master password is still the old one and manual recovery (e.g.
// restoring from backup) is required.
//
// Returns the number of credentials re-encrypted (zero unless the rotation
// completed end-to-end).
func (cs *CredentialStore) RotateMasterPassword(ctx context.Context, oldPW, newPW string) (count int, err error) {
	if oldPW == "" || newPW == "" {
		return 0, ErrEmptyMasterPassword
	}

	// Verify old password matches current master password.
	if !bytes.Equal([]byte(oldPW), cs.masterPassword) {
		return 0, ErrMasterPasswordMismatch
	}

	// List all credentials.
	creds, err := cs.store.ListCredentials(ctx)
	if err != nil {
		return 0, fmt.Errorf("credential: list for master password rotation: %w", err)
	}

	if len(creds) == 0 {
		// No credentials to rotate; just update the master password.
		newMP := make([]byte, len(newPW))
		copy(newMP, newPW)
		SecureZero(cs.masterPassword)
		cs.masterPassword = newMP
		return 0, nil
	}

	// Phase 1: Decrypt all credentials with old password to verify they work.
	plaintexts := make(map[string][]byte, len(creds))
	for _, cred := range creds {
		pt, err := cs.decrypt(cred.EncryptedData)
		if err != nil {
			// Abort before touching anything: cannot decrypt this credential
			// with the old password.
			for _, p := range plaintexts {
				SecureZero(p)
			}
			return 0, fmt.Errorf("credential: decrypt %q during master rotation: %w", cred.Name, err)
		}
		plaintexts[cred.ID] = pt
	}

	// Phase 2a: Pre-compute ALL new ciphertexts so an encryption failure
	// aborts before any row is modified.
	newMP := make([]byte, len(newPW))
	copy(newMP, newPW)

	tempStore := &CredentialStore{
		store:          cs.store,
		masterPassword: newMP,
		timeCost:       cs.timeCost,
		memoryCost:     cs.memoryCost,
		parallelism:    cs.parallelism,
	}

	newBlobs := make(map[string][]byte, len(creds))
	for _, cred := range creds {
		blob, err := tempStore.encrypt(plaintexts[cred.ID])
		if err != nil {
			for _, p := range plaintexts {
				SecureZero(p)
			}
			SecureZero(newMP)
			for _, b := range newBlobs {
				SecureZero(b)
			}
			return 0, fmt.Errorf("credential: re-encrypt %q during master rotation: %w", cred.Name, err)
		}
		newBlobs[cred.ID] = blob
	}

	// Phase 2b: Persist each re-encrypted blob in order, recording enough
	// state to roll back the already-persisted prefix on failure.
	undo := make([]rotationUndo, 0, len(creds))
	for _, cred := range creds {
		oldBlob := cred.EncryptedData
		cred.EncryptedData = newBlobs[cred.ID]
		if err := cs.store.UpdateCredential(ctx, cred); err != nil {
			cred.EncryptedData = oldBlob // restore the in-memory snapshot
			perr := fmt.Errorf("credential: persist %q during master rotation: %w", cred.Name, err)
			err := cs.rollbackMasterRotation(ctx, undo)
			// Zero sensitive material regardless of outcome.
			for _, p := range plaintexts {
				SecureZero(p)
			}
			SecureZero(newMP)
			for _, b := range newBlobs {
				SecureZero(b)
			}
			if err != nil {
				return 0, fmt.Errorf("%v; %v", perr, err)
			}
			return 0, fmt.Errorf("%v; %v", perr,
				fmt.Sprintf("rolled back %d credential(s) to their previous ciphertext; master password unchanged", len(undo)))
		}
		undo = append(undo, rotationUndo{cred: cred, oldBlob: oldBlob})
	}

	// Phase 3: Update in-memory master password only after every row has
	// been persisted successfully.
	SecureZero(cs.masterPassword)
	cs.masterPassword = newMP

	log.InfoCtx(ctx, "master password rotated", "credentials_affected", len(creds))
	return len(creds), nil
}

// rollbackMasterRotation re-persists the pre-rotation ciphertext for every
// entry in undo. It returns a fatal error enumerating the credential ids for
// which even the rollback failed; those rows are left with new-password
// ciphertext while the active master password is still the old one.
func (cs *CredentialStore) rollbackMasterRotation(ctx context.Context, undo []rotationUndo) error {
	var unrecoverable []string
	for _, u := range undo {
		u.cred.EncryptedData = u.oldBlob
		if err := cs.store.UpdateCredential(ctx, u.cred); err != nil {
			unrecoverable = append(unrecoverable, u.cred.ID)
			log.ErrorCtx(ctx, "master rotation rollback failed",
				"credential_id", u.cred.ID, "err", err)
		}
	}
	if len(unrecoverable) > 0 {
		return fmt.Errorf("credential: FATAL: rollback failed for %d credential(s) %v — rows still hold NEW-password ciphertext while the active master password is unchanged; restore them from backup",
			len(unrecoverable), unrecoverable)
	}
	return nil
}

// --- ID generation ----------------------------------------------------------

// newID generates a unique credential identifier using crypto/rand. The
// ID has the form "cred-<16-hex-chars>". On the extremely unlikely event
// that rand.Read fails, it falls back to a timestamp-based ID so the
// caller always gets a usable, unique-enough identifier.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("cred-%d", time.Now().UnixNano())
	}
	return "cred-" + hex.EncodeToString(b)
}
