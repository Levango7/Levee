// CredentialProvider resolves credentials on demand during the apply phase.
// This file extends the credential package (see store.go) with a higher-level
// abstraction that adds:
//   - target→credential name mapping (loaded from config or registered at runtime)
//   - use-and-discard semantics (Get returns plaintext; Clear zeros it)
//   - structured failure (R7 error code wrapping)
//
// Security: the plaintext returned by Get lives only in the caller's memory
// until Clear is invoked. Clear overwrites every byte and calls
// runtime.KeepAlive so the zeroing is not optimized away. Plaintext is never
// logged and never included in error messages.

package credential

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// --- Provider sentinel errors ----------------------------------------------

var (
	// ErrTargetNotRegistered is returned when a target has no credential
	// mapping registered with the CredentialProvider.
	ErrTargetNotRegistered = errors.New("credential: target not registered")

	// ErrCredentialNotFound is returned when the credential mapped to a
	// target does not exist in the CredentialStore.
	ErrCredentialNotFound = errors.New("credential: credential not found")

	// ErrTypeMismatch is returned by GetByType when the resolved credential
	// type does not match the requested type.
	ErrTypeMismatch = errors.New("credential: type mismatch")

	// ErrNilStore is returned when NewCredentialProvider is called with a
	// nil CredentialStore.
	ErrNilStore = errors.New("credential: nil credential store")
)

// --- CredentialStore metadata helper ----------------------------------------
//
// GetMetadata is defined here (not in store.go) to avoid modifying the
// existing T047 file. It exposes the stored metadata — including the Type
// field — without decrypting the ciphertext, which the CredentialProvider
// needs to populate ResolvedCredential.Type.

// GetMetadata returns the stored metadata for the credential with the given
// name without decrypting it. The returned state.Credential contains the
// Type field but its EncryptedData should be treated as opaque.
//
// Returns ErrEmptyName when name is empty, ErrNotFound when the credential
// does not exist.
func (cs *CredentialStore) GetMetadata(ctx context.Context, name string) (*state.Credential, error) {
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
	return cred, nil
}

// --- ResolvedCredential -----------------------------------------------------

// ResolvedCredential is a fully resolved credential ready for use by an
// executor. The Plaintext field contains the decrypted bytes; the caller
// MUST call CredentialProvider.Clear when done to zero them from memory
// (use-and-discard semantics).
type ResolvedCredential struct {
	Target     string    // 目标机
	Name       string    // 凭据名
	Type       string    // 凭据类型
	Plaintext  []byte    // 明文（调用方负责 Clear）
	ResolvedAt time.Time // 解析时间
}

// --- CredentialProvider -----------------------------------------------------

// CredentialProvider resolves credentials on demand during the apply phase.
// It wraps CredentialStore with:
//   - target→credential name mapping (loaded from config or registered at runtime)
//   - use-and-discard semantics (Get returns plaintext; Clear zeros it)
//   - structured failure (R7 error code wrapping)
//
// The targetCredMap is guarded by mu so RegisterTarget/UnregisterTarget may
// be called concurrently with Get/ListTargets.
type CredentialProvider struct {
	store *CredentialStore

	mu            sync.RWMutex
	targetCredMap map[string]string // target → credential name
}

// NewCredentialProvider creates a new CredentialProvider backed by the given
// CredentialStore. The targetMap maps target host names to credential names;
// it is copied so the caller may mutate the original without affecting the
// provider. Additional mappings can be added via RegisterTarget.
//
// store must be non-nil; otherwise ErrNilStore is returned.
func NewCredentialProvider(store *CredentialStore, targetMap map[string]string) (*CredentialProvider, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	m := make(map[string]string, len(targetMap))
	for k, v := range targetMap {
		m[k] = v
	}
	return &CredentialProvider{
		store:         store,
		targetCredMap: m,
	}, nil
}

// Get resolves the credential for the given target. It looks up the
// target→credential name mapping, retrieves and decrypts the credential
// from the CredentialStore, and returns a ResolvedCredential containing
// the plaintext.
//
// The caller MUST call Clear on the returned ResolvedCredential when done
// to zero the plaintext from memory.
//
// Failure modes (R7 structured, all wrapped as
// "credential: resolve failed for target <target>: <cause>"):
//   - empty target: wraps ErrTargetNotRegistered
//   - target not registered: wraps ErrTargetNotRegistered
//   - credential not in store: wraps ErrCredentialNotFound
//   - decrypt / store failure: wraps the underlying error
func (p *CredentialProvider) Get(ctx context.Context, target string) (*ResolvedCredential, error) {
	if target == "" {
		return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, ErrTargetNotRegistered)
	}

	p.mu.RLock()
	credName, ok := p.targetCredMap[target]
	p.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, ErrTargetNotRegistered)
	}

	// Fetch metadata first to confirm existence and obtain the Type. This
	// also gives a clean ErrNotFound before we attempt decryption.
	meta, err := p.store.GetMetadata(ctx, credName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, ErrCredentialNotFound)
		}
		return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, err)
	}

	plaintext, err := p.store.Retrieve(ctx, credName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, ErrCredentialNotFound)
		}
		return nil, fmt.Errorf("credential: resolve failed for target %q: %w", target, err)
	}

	// Log only the target and credential name; never the plaintext.
	log.DebugCtx(ctx, "credential resolved for target", "target", target, "name", credName)

	return &ResolvedCredential{
		Target:     target,
		Name:       credName,
		Type:       meta.Type,
		Plaintext:  plaintext,
		ResolvedAt: time.Now().UTC(),
	}, nil
}

// GetByType resolves the credential for the given target and verifies that
// its stored type matches credType. This is used when the caller requires a
// specific credential type (e.g. ssh_key) for a target.
//
// On type mismatch the resolved credential is cleared before returning the
// error, so no plaintext leaks to the caller.
//
// Returns ErrTypeMismatch (wrapped) when the resolved credential type does
// not match credType.
func (p *CredentialProvider) GetByType(ctx context.Context, target, credType string) (*ResolvedCredential, error) {
	cred, err := p.Get(ctx, target)
	if err != nil {
		return nil, err
	}
	if cred.Type != credType {
		// Clear before returning so plaintext does not leak on mismatch.
		p.Clear(cred)
		return nil, fmt.Errorf("credential: resolve failed for target %q: expected type %q, got %q: %w",
			target, credType, cred.Type, ErrTypeMismatch)
	}
	return cred, nil
}

// Clear zeros the Plaintext of a ResolvedCredential and releases the
// reference. Call this as soon as the plaintext is no longer needed
// (use-and-discard semantics).
//
// Clear is safe to call on a nil credential or one with nil/empty Plaintext.
// It calls runtime.KeepAlive on the slice so the compiler does not optimize
// away the zeroing.
func (p *CredentialProvider) Clear(cred *ResolvedCredential) {
	if cred == nil {
		return
	}
	SecureZero(cred.Plaintext)
	// KeepAlive prevents the compiler from eliding the zeroing above.
	runtime.KeepAlive(cred.Plaintext)
	cred.Plaintext = nil
}

// RegisterTarget adds or updates a target→credential name mapping.
// If the target already exists, its mapping is overwritten.
// This is safe to call concurrently with Get and ListTargets.
func (p *CredentialProvider) RegisterTarget(target, credName string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targetCredMap[target] = credName
}

// UnregisterTarget removes a target→credential name mapping.
// It is a no-op if the target is not registered.
// This is safe to call concurrently with Get and ListTargets.
func (p *CredentialProvider) UnregisterTarget(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.targetCredMap, target)
}

// ListTargets returns all registered target host names, sorted for
// deterministic output. The returned slice is a copy; mutating it does not
// affect the provider's internal state.
func (p *CredentialProvider) ListTargets() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	targets := make([]string, 0, len(p.targetCredMap))
	for k := range p.targetCredMap {
		targets = append(targets, k)
	}
	sort.Strings(targets)
	return targets
}
