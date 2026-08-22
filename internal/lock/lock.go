// Package lock implements target-host level mutual exclusion locks with
// TTL for LEVEE's change pipeline. Locks prevent two concurrent runs from
// operating on the same target host simultaneously, which is a hard
// safety requirement when applying mutating actions (patch, restart,
// config change) to production hosts.
//
// Key features:
//   - TTL-based expiration (default 1 hour): a lock that has not been
//     refreshed or released within its TTL is considered expired and may
//     be preempted by a new owner.
//   - Automatic preemption: when LockManager.Acquire is called on a
//     target whose lock has expired, the lock is automatically taken
//     over by the new owner and the preemption is recorded as an audit
//     entry.
//   - Pre-acquire state check: LockManager.CheckAndAcquire verifies that
//     the target has no in-flight steps (status "running" or "pending")
//     before acquiring the lock, refusing to preempt a busy target.
//
// The package is structured in two layers:
//
//   - LockStore is the low-level persistence abstraction. Its Acquire is
//     a plain "create if absent" operation; it does not perform automatic
//     preemption. ForceAcquire unconditionally takes over a lock.
//   - LockManager is the high-level orchestrator. Its Acquire implements
//     the "auto-preempt expired lock" policy on top of LockStore, and
//     CheckAndAcquire adds the target-busy guard.
//
// Locks are persisted to SQLite via state.Store. The scope convention is
// "host:<target>", matching the locks table UNIQUE(scope) constraint.
package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// DefaultTTL is the default time-to-live for a lock when no TTL is
// specified (1 hour). A lock that has not been refreshed or released
// within this duration is considered expired and may be preempted.
const DefaultTTL = 1 * time.Hour

// --- Sentinel errors --------------------------------------------------------

var (
	// ErrLockHeld is returned when the target is already locked by another
	// owner and the lock has not yet expired. Callers should retry later
	// or report the conflict to the operator.
	ErrLockHeld = errors.New("lock: target is already locked")

	// ErrLockNotFound is returned when the target has no lock to release.
	ErrLockNotFound = errors.New("lock: no lock found for target")

	// ErrNotOwner is returned when the caller attempts to release a lock
	// they do not own. This protects against one run accidentally
	// releasing another run's lock.
	ErrNotOwner = errors.New("lock: caller is not the lock owner")

	// ErrTargetBusy is returned when the target has in-flight steps
	// (status "running" or "pending") and cannot be preempted safely.
	// The operator must wait for the in-flight work to finish or abort it
	// before retrying.
	ErrTargetBusy = errors.New("lock: target has in-flight work, cannot preempt")

	// ErrEmptyTarget is returned when the target identifier is empty.
	ErrEmptyTarget = errors.New("lock: empty target")

	// ErrEmptyOwner is returned when the owner identifier is empty.
	ErrEmptyOwner = errors.New("lock: empty owner")
)

// --- Lock -------------------------------------------------------------------

// Lock is a target-host level mutex lock with a TTL. The Target field
// identifies the host being locked; the Owner field identifies the run
// holding the lock (typically a RunID). AcquiredAt and ExpiresAt record
// the acquisition time and the expiration deadline respectively. TTL is
// the lock's time-to-live duration.
type Lock struct {
	Target     string        `json:"target"`
	Owner      string        `json:"owner"`
	AcquiredAt time.Time     `json:"acquired_at"`
	ExpiresAt  time.Time     `json:"expires_at"`
	TTL        time.Duration `json:"ttl"`
}

// Expired reports whether the lock has expired relative to the given
// timestamp. A nil lock or a lock with a zero ExpiresAt never expires.
// A lock is considered expired when now is at or after ExpiresAt.
func (l *Lock) Expired(now time.Time) bool {
	if l == nil {
		return false
	}
	if l.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(l.ExpiresAt)
}

// --- LockStore interface ----------------------------------------------------

// LockStore is the persistence abstraction for target-host locks.
// Implementations must be safe for concurrent use.
//
// Conventions:
//   - Acquire creates a new lock when the target is unlocked and returns
//     ErrLockHeld when the target is already locked, regardless of
//     expiration. It does NOT perform automatic preemption; use
//     ForceAcquire or LockManager.Acquire for that.
//   - Release returns ErrLockNotFound when the target has no lock and
//     ErrNotOwner when the lock is held by a different owner.
//   - Get returns (nil, nil) when the target has no lock.
//   - ForceAcquire unconditionally takes over the lock, regardless of
//     whether the existing lock has expired or who owns it. It is
//     intended for administrative use and for automatic preemption of
//     expired locks by the LockManager.
//   - ListExpired returns all locks whose ExpiresAt is in the past.
type LockStore interface {
	Acquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error)
	Release(ctx context.Context, target, owner string) error
	Get(ctx context.Context, target string) (*Lock, error)
	ForceAcquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error)
	ListExpired(ctx context.Context) ([]*Lock, error)
}

// --- stateLockStore ---------------------------------------------------------

// stateLockStore adapts state.Store to the LockStore interface. It uses
// the "host:<target>" scope convention so locks are namespaced by target
// host, matching the UNIQUE(scope) constraint on the locks table.
type stateLockStore struct {
	store state.Store
}

// NewLockStore returns a LockStore backed by the given state.Store. The
// state.Store must be non-nil.
func NewLockStore(store state.Store) LockStore {
	return &stateLockStore{store: store}
}

// scope returns the state.Lock scope for the given target host.
func scope(target string) string {
	return "host:" + target
}

// toStateLock converts a lock.Lock to a state.Lock, generating a new ID.
func toStateLock(l *Lock) *state.Lock {
	return &state.Lock{
		ID:         newID(),
		Scope:      scope(l.Target),
		Owner:      l.Owner,
		TTLSeconds: int(l.TTL / time.Second),
		AcquiredAt: l.AcquiredAt,
		ExpiresAt:  l.ExpiresAt,
	}
}

// fromStateLock converts a state.Lock to a lock.Lock. The scope is
// expected to have the "host:" prefix; any other prefix is preserved as-
// is in the Target field so no information is lost.
func fromStateLock(sl *state.Lock) *Lock {
	return &Lock{
		Target:     strings.TrimPrefix(sl.Scope, "host:"),
		Owner:      sl.Owner,
		AcquiredAt: sl.AcquiredAt,
		ExpiresAt:  sl.ExpiresAt,
		TTL:        time.Duration(sl.TTLSeconds) * time.Second,
	}
}

// Acquire attempts to acquire a lock on the target. If the target has no
// lock, a new lock is created. If the target already has a lock
// (regardless of expiration status), ErrLockHeld is returned. Use
// ForceAcquire or LockManager.Acquire for preemption.
//
// A non-positive ttl is replaced with DefaultTTL.
func (s *stateLockStore) Acquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	existing, err := s.store.GetLockByScope(ctx, scope(target))
	if err != nil {
		return nil, fmt.Errorf("lock: get existing: %w", err)
	}
	if existing != nil {
		return nil, fmt.Errorf("%w: target %s owned by %s", ErrLockHeld, target, existing.Owner)
	}

	now := time.Now().UTC()
	l := &Lock{
		Target:     target,
		Owner:      owner,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
		TTL:        ttl,
	}
	sl := toStateLock(l)
	if err := s.store.CreateLock(ctx, sl); err != nil {
		return nil, fmt.Errorf("lock: create: %w", err)
	}
	return fromStateLock(sl), nil
}

// Release releases the lock on the target. Only the owner may release
// the lock; otherwise ErrNotOwner is returned. Returns ErrLockNotFound
// when the target has no lock.
func (s *stateLockStore) Release(ctx context.Context, target, owner string) error {
	if target == "" {
		return ErrEmptyTarget
	}
	existing, err := s.store.GetLockByScope(ctx, scope(target))
	if err != nil {
		return fmt.Errorf("lock: get for release: %w", err)
	}
	if existing == nil {
		return ErrLockNotFound
	}
	if existing.Owner != owner {
		return fmt.Errorf("%w: target %s owned by %s, not %s", ErrNotOwner, target, existing.Owner, owner)
	}
	if err := s.store.DeleteLock(ctx, existing.ID); err != nil {
		return fmt.Errorf("lock: delete: %w", err)
	}
	return nil
}

// Get returns the lock on the target, or (nil, nil) if no lock exists.
func (s *stateLockStore) Get(ctx context.Context, target string) (*Lock, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	existing, err := s.store.GetLockByScope(ctx, scope(target))
	if err != nil {
		return nil, fmt.Errorf("lock: get: %w", err)
	}
	if existing == nil {
		return nil, nil
	}
	return fromStateLock(existing), nil
}

// ForceAcquire unconditionally takes over the lock on the target,
// regardless of whether the existing lock has expired or who owns it.
// If no lock exists, a new one is created. It is intended for
// administrative use and for automatic preemption of expired locks by
// the LockManager.
//
// A non-positive ttl is replaced with DefaultTTL.
func (s *stateLockStore) ForceAcquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	now := time.Now().UTC()
	existing, err := s.store.GetLockByScope(ctx, scope(target))
	if err != nil {
		return nil, fmt.Errorf("lock: get for force acquire: %w", err)
	}

	if existing != nil {
		// Guard against non-expired locks: do not let ForceAcquire
		// preempt a still-valid lock held by another owner. This
		// matches the LockManager policy (only preempt expired locks)
		// and avoids silent data corruption when two actors race.
		if !now.After(existing.ExpiresAt) && !now.Equal(existing.ExpiresAt) {
			return nil, ErrLockHeld
		}
		// Use a conditional UPDATE (WHERE id=? AND expires_at<=now)
		// so that a concurrent race on an expired lock is detected:
		// RowsAffected()==0 means another actor already won the
		// update and we retry.
		rows, err := s.store.UpdateLockOwnedBy(ctx, existing.ID, owner, int(ttl/time.Second), now)
		if err != nil {
			return nil, fmt.Errorf("lock: force acquire update: %w", err)
		}
		if rows == 0 {
			// Another actor raced us; retry from scratch.
			return s.ForceAcquire(ctx, target, owner, ttl)
		}
		// Re-read to get the canonical state after the conditional
		// update (the update touches acquired_at/expires_at).
		l, err := s.store.GetLockByScope(ctx, scope(target))
		if err != nil {
			return nil, fmt.Errorf("lock: force acquire re-read: %w", err)
		}
		return fromStateLock(l), nil
	}

	l := &Lock{
		Target:     target,
		Owner:      owner,
		AcquiredAt: now,
		ExpiresAt:  now.Add(ttl),
		TTL:        ttl,
	}
	sl := toStateLock(l)
	if err := s.store.CreateLock(ctx, sl); err != nil {
		return nil, fmt.Errorf("lock: force acquire create: %w", err)
	}
	return fromStateLock(sl), nil
}

// ListExpired returns all locks whose ExpiresAt is in the past relative
// to the current time. The returned slice is nil when no locks are
// expired.
func (s *stateLockStore) ListExpired(ctx context.Context) ([]*Lock, error) {
	all, err := s.store.ListLocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock: list: %w", err)
	}
	now := time.Now().UTC()
	var expired []*Lock
	for _, sl := range all {
		if now.After(sl.ExpiresAt) {
			expired = append(expired, fromStateLock(sl))
		}
	}
	return expired, nil
}

// --- LockManager ------------------------------------------------------------

// LockManager orchestrates lock acquisition with automatic expiration
// handling and pre-acquire state checks. It wraps a LockStore and a
// state.Store: the LockStore provides lock CRUD, while the state.Store
// is used to check whether a target has in-flight steps before
// preempting an expired lock and to record audit entries for
// preemptions.
//
// A LockManager is safe for concurrent use provided the underlying
// LockStore and state.Store are.
type LockManager struct {
	store      LockStore
	state      state.Store
	defaultTTL time.Duration
}

// NewLockManager returns a LockManager backed by the given LockStore and
// state.Store. Both must be non-nil. The default TTL is DefaultTTL (1h);
// override it with SetTTL.
func NewLockManager(store LockStore, st state.Store) *LockManager {
	return &LockManager{
		store:      store,
		state:      st,
		defaultTTL: DefaultTTL,
	}
}

// SetTTL overrides the default TTL for locks acquired through this
// manager. A non-positive ttl is ignored. This method is intended for
// configuration during start-up; calling it concurrently with Acquire is
// not safe.
func (m *LockManager) SetTTL(ttl time.Duration) {
	if ttl > 0 {
		m.defaultTTL = ttl
	}
}

// Acquire attempts to acquire a lock on the target. Behaviour:
//   - No existing lock: a new lock is acquired.
//   - Existing unexpired lock held by another owner: returns ErrLockHeld.
//   - Existing expired lock: the lock is automatically preempted by the
//     new owner. The preemption is recorded as an audit entry in the
//     state.Store.
//
// On success the returned *Lock is the newly acquired (or preempted)
// lock with a fresh AcquiredAt and ExpiresAt.
func (m *LockManager) Acquire(ctx context.Context, target, owner string) (*Lock, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	if owner == "" {
		return nil, ErrEmptyOwner
	}

	existing, err := m.store.Get(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("lock: acquire get: %w", err)
	}

	if existing == nil {
		l, err := m.store.Acquire(ctx, target, owner, m.defaultTTL)
		if err != nil {
			return nil, fmt.Errorf("lock: acquire: %w", err)
		}
		log.InfoCtx(ctx, "lock acquired",
			"target", target, "owner", owner, "expires_at", l.ExpiresAt)
		return l, nil
	}

	now := time.Now().UTC()
	if !existing.Expired(now) {
		return nil, fmt.Errorf("%w: target %s owned by %s", ErrLockHeld, target, existing.Owner)
	}

	// Expired — preempt via ForceAcquire.
	l, err := m.store.ForceAcquire(ctx, target, owner, m.defaultTTL)
	if err != nil {
		return nil, fmt.Errorf("lock: preempt: %w", err)
	}

	// Record audit entry for the preemption. The audit write is best-
	// effort: a failure is logged but does not undo the preemption,
	// because the preemption is the safety-critical action and the audit
	// entry is observability.
	m.recordAudit(ctx, target, existing.Owner, owner)

	log.InfoCtx(ctx, "lock preempted (expired)",
		"target", target, "old_owner", existing.Owner, "new_owner", owner,
		"expires_at", l.ExpiresAt)
	return l, nil
}

// Release releases the lock on the target. Only the owner may release
// the lock; otherwise ErrNotOwner is returned (wrapped). Returns
// ErrLockNotFound (wrapped) when the target has no lock.
func (m *LockManager) Release(ctx context.Context, target, owner string) error {
	if err := m.store.Release(ctx, target, owner); err != nil {
		return fmt.Errorf("lock: release: %w", err)
	}
	log.InfoCtx(ctx, "lock released", "target", target, "owner", owner)
	return nil
}

// CheckAndAcquire checks that the target has no in-flight steps before
// acquiring the lock. If the target has in-flight steps (status
// "running" or "pending"), ErrTargetBusy is returned and no lock is
// acquired. Otherwise the lock is acquired with the same semantics as
// Acquire (including automatic preemption of expired locks).
//
// This method is the safe entry point for lock acquisition: it refuses
// to preempt a lock on a target that still has live work, which could
// otherwise lead to two runs operating on the same host concurrently.
func (m *LockManager) CheckAndAcquire(ctx context.Context, target, owner string) (*Lock, error) {
	if target == "" {
		return nil, ErrEmptyTarget
	}
	if owner == "" {
		return nil, ErrEmptyOwner
	}

	busy, err := m.isTargetBusy(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("lock: check target busy: %w", err)
	}
	if busy {
		return nil, fmt.Errorf("%w: target %s", ErrTargetBusy, target)
	}

	return m.Acquire(ctx, target, owner)
}

// CleanExpired removes all expired locks from the store and returns the
// number of locks removed. A lock is expired when its ExpiresAt is in
// the past relative to the current time.
func (m *LockManager) CleanExpired(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	n, err := m.state.DeleteExpiredLocks(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("lock: clean expired: %w", err)
	}
	if n > 0 {
		log.InfoCtx(ctx, "expired locks cleaned", "count", n)
	}
	return int(n), nil
}

// --- helpers ----------------------------------------------------------------

// isTargetBusy reports whether the target host has any in-flight steps
// (status "running" or "pending"). A target with in-flight steps must
// not be preempted, because the owning run is still actively executing
// work on it.
func (m *LockManager) isTargetBusy(ctx context.Context, target string) (bool, error) {
	running, err := m.state.ListSteps(ctx, state.StepFilter{Host: target, Status: "running"})
	if err != nil {
		return false, fmt.Errorf("list running steps: %w", err)
	}
	if len(running) > 0 {
		return true, nil
	}
	pending, err := m.state.ListSteps(ctx, state.StepFilter{Host: target, Status: "pending"})
	if err != nil {
		return false, fmt.Errorf("list pending steps: %w", err)
	}
	return len(pending) > 0, nil
}

// recordAudit writes an audit entry for a lock preemption. The audit
// entry records the new owner as the actor and the target as the target;
// the action is "lock" and the result is "success". Errors are logged
// but not returned, because failing to write the audit entry should not
// prevent the preemption itself (the preemption is the safety-critical
// action; the audit entry is observability).
func (m *LockManager) recordAudit(ctx context.Context, target, oldOwner, newOwner string) {
	audit := &state.Audit{
		ID:        newID(),
		RunID:     newOwner,
		Action:    "lock",
		Actor:     newOwner,
		Target:    target,
		Result:    "success",
		Timestamp: time.Now().UTC(),
	}
	if err := m.state.CreateAudit(ctx, audit); err != nil {
		log.WarnCtx(ctx, "lock audit write failed",
			"target", target, "old_owner", oldOwner, "new_owner", newOwner, "err", err)
		return
	}
	log.DebugCtx(ctx, "lock preemption audited",
		"target", target, "old_owner", oldOwner, "new_owner", newOwner)
}

// newID generates a unique lock identifier using crypto/rand. The ID has
// the form "lock-<16-hex-chars>". On the extremely unlikely event that
// rand.Read fails, it falls back to a timestamp-based ID so the caller
// always gets a usable, unique-enough identifier.
func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("lock-%d", time.Now().UnixNano())
	}
	return "lock-" + hex.EncodeToString(b)
}
