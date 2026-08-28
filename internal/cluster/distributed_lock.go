// distributed_lock.go implements a distributed mutex backed by a PostgreSQL
// lease table (cluster_locks).
//
// Each lock row records its owner and a lease expiry. Acquire is a single
// atomic INSERT ... ON CONFLICT statement: it succeeds when the key is free,
// when the caller already owns it (lease refresh), or when the current
// lease has expired (reclaim). A holder keeps its lease alive by calling
// Refresh periodically; a node that crashes or loses database connectivity
// stops refreshing, so its lease expires and another node can take the lock
// over. This is the property advisory locks cannot provide: they are
// released on disconnect but never expire, so a hung-but-connected holder
// would block the key forever.
//
// Every successful acquisition (or reclaim) carries a strictly increasing
// fence_token drawn from a dedicated sequence. Work guarded by a lock should
// persist the fence token with its state transitions so that a zombie holder
// that wakes up after its lease was reclaimed cannot corrupt work owned by
// the new holder (fencing).
//
// The manager also keeps an in-process record of the locks this process
// holds so IsHeld / ListHeld / ReleaseAll do not need a database round-trip;
// the table remains the source of truth for cross-node visibility.

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// ErrLockNotHeld is returned when an operation requires the caller to hold a
// lock but the lock is not held by the given owner (or its lease was lost).
var ErrLockNotHeld = errors.New("cluster: lock not held")

// ErrLockBusy is returned when Acquire fails because another owner holds the
// lock with a live lease. Callers should retry with backoff.
var ErrLockBusy = errors.New("cluster: lock busy")

// DistributedLock represents a held lease lock. Key/Owner/TTL/AcquiredAt
// mirror the table row; FenceToken is the monotonic acquisition sequence
// value for this holding of the lock.
type DistributedLock struct {
	Key         string // caller-supplied lock key
	Owner       string // owner identifier (e.g. node ID)
	TTL         time.Duration
	FenceToken  int64
	AcquiredAt  time.Time
	LeaseExpiry time.Time
}

// ReleasedLock describes a lock row removed by ReleaseStale.
type ReleasedLock struct {
	Key   string
	Owner string
}

// DistributedLockManager coordinates PostgreSQL lease locks. It is safe for
// concurrent use. A single manager should be shared by all goroutines in a
// process so that held-lock state is centralised.
type DistributedLockManager struct {
	db *sql.DB

	mu   sync.RWMutex
	held map[string]*DistributedLock // key -> lock (this process only)
}

// NewDistributedLockManager returns a manager backed by the given *sql.DB.
// The db must point at the same PostgreSQL instance used by the PGStore. A
// nil db yields a manager whose Acquire/Refresh/ReleaseStale calls fail —
// useful for in-process tests of the surrounding lifecycle.
func NewDistributedLockManager(db *sql.DB) *DistributedLockManager {
	return &DistributedLockManager{db: db, held: make(map[string]*DistributedLock)}
}

// acquireLockSQL atomically claims a lock row. The INSERT path covers a free
// key; the ON CONFLICT update fires only when the caller already owns the
// row (lease refresh, fence token kept) or the lease has expired (reclaim,
// fresh fence token). When neither condition holds the UPDATE is suppressed
// by its WHERE clause and no row is returned — the caller sees ErrLockBusy.
//
// Sequence gaps are expected and harmless: the VALUES expression consumes a
// fence token even on the conflict path. Monotonicity is what matters.
const acquireLockSQL = `
INSERT INTO cluster_locks (key, owner, fence_token, acquired_at, lease_expires)
VALUES ($1, $2, nextval('cluster_locks_fence_seq'), NOW(), NOW() + ($3::bigint * INTERVAL '1 millisecond'))
ON CONFLICT (key) DO UPDATE
SET owner = EXCLUDED.owner,
    fence_token = CASE WHEN cluster_locks.owner = EXCLUDED.owner
                       THEN cluster_locks.fence_token
                       ELSE nextval('cluster_locks_fence_seq') END,
    acquired_at = CASE WHEN cluster_locks.owner = EXCLUDED.owner
                       THEN cluster_locks.acquired_at
                       ELSE NOW() END,
    lease_expires = NOW() + ($3::bigint * INTERVAL '1 millisecond')
WHERE cluster_locks.owner = EXCLUDED.owner OR cluster_locks.lease_expires < NOW()
RETURNING fence_token, acquired_at, lease_expires`

// Acquire obtains an exclusive lease lock on key for owner with the given
// lease ttl. If another owner holds the key with a live lease it returns
// ErrLockBusy. Re-acquiring a key already owned by the same owner refreshes
// the lease and keeps the existing fence token.
//
// The lease is only valid while it is refreshed: once it expires, any other
// owner may reclaim the key. Holders must call Refresh at an interval well
// below ttl (the ClusterManager does this for its own locks).
func (m *DistributedLockManager) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (*DistributedLock, error) {
	if key == "" {
		return nil, fmt.Errorf("cluster: acquire: empty key")
	}
	if owner == "" {
		return nil, fmt.Errorf("cluster: acquire: empty owner")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("cluster: acquire: non-positive ttl")
	}
	if m.db == nil {
		return nil, fmt.Errorf("cluster: acquire: nil db")
	}

	var fence int64
	var acquiredAt, expiry time.Time
	err := m.db.QueryRowContext(ctx, acquireLockSQL, key, owner, ttl.Milliseconds()).
		Scan(&fence, &acquiredAt, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrLockBusy
	}
	if err != nil {
		return nil, fmt.Errorf("cluster: acquire: %w", err)
	}

	lock := &DistributedLock{
		Key:         key,
		Owner:       owner,
		TTL:         ttl,
		FenceToken:  fence,
		AcquiredAt:  acquiredAt,
		LeaseExpiry: expiry,
	}

	m.mu.Lock()
	m.held[key] = lock
	m.mu.Unlock()
	log.Debug("distributed lock acquired", "key", key, "owner", owner, "fence_token", fence)
	return lock, nil
}

// Release frees the lock held by owner on key. If the lock is not held by
// owner (or the lease was already reclaimed) it returns ErrLockNotHeld.
func (m *DistributedLockManager) Release(ctx context.Context, key, owner string) error {
	if key == "" {
		return fmt.Errorf("cluster: release: empty key")
	}
	if m.db == nil {
		return fmt.Errorf("cluster: release: nil db")
	}

	res, err := m.db.ExecContext(ctx,
		`DELETE FROM cluster_locks WHERE key = $1 AND owner = $2`, key, owner)
	if err != nil {
		return fmt.Errorf("cluster: release: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cluster: release: rows affected: %w", err)
	}

	m.mu.Lock()
	if held, ok := m.held[key]; ok && held.Owner == owner {
		delete(m.held, key)
	}
	m.mu.Unlock()

	if n == 0 {
		return ErrLockNotHeld
	}
	log.Debug("distributed lock released", "key", key, "owner", owner)
	return nil
}

// Refresh extends the lease of a held lock. It returns ErrLockNotHeld when
// the lock is not held by owner or the lease has already expired — in both
// cases the caller must stop acting on the assumption that it holds the
// lock and re-acquire if appropriate.
func (m *DistributedLockManager) Refresh(ctx context.Context, key, owner string, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cluster: refresh: non-positive ttl")
	}
	if m.db == nil {
		return fmt.Errorf("cluster: refresh: nil db")
	}

	res, err := m.db.ExecContext(ctx,
		`UPDATE cluster_locks
		 SET lease_expires = NOW() + ($3::bigint * INTERVAL '1 millisecond')
		 WHERE key = $1 AND owner = $2 AND lease_expires >= NOW()`,
		key, owner, ttl.Milliseconds())
	if err != nil {
		return fmt.Errorf("cluster: refresh: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("cluster: refresh: rows affected: %w", err)
	}
	if n == 0 {
		// Lease lost: drop local state so IsHeld reflects reality.
		m.mu.Lock()
		if held, ok := m.held[key]; ok && held.Owner == owner {
			delete(m.held, key)
		}
		m.mu.Unlock()
		return ErrLockNotHeld
	}

	m.mu.Lock()
	if held, ok := m.held[key]; ok && held.Owner == owner {
		held.TTL = ttl
		held.LeaseExpiry = time.Now().UTC().Add(ttl)
	}
	m.mu.Unlock()
	return nil
}

// ReleaseStale removes every lock row whose lease has expired and returns
// the removed entries. It is idempotent: rows already reclaimed by a new
// Acquire are not reported. Intended for the ClusterManager health loop and
// for operational hygiene; correctness does not depend on it because
// Acquire can always reclaim an expired row itself.
func (m *DistributedLockManager) ReleaseStale(ctx context.Context) ([]ReleasedLock, error) {
	if m.db == nil {
		return nil, fmt.Errorf("cluster: release stale: nil db")
	}
	rows, err := m.db.QueryContext(ctx,
		`DELETE FROM cluster_locks WHERE lease_expires < NOW() RETURNING key, owner`)
	if err != nil {
		return nil, fmt.Errorf("cluster: release stale: %w", err)
	}
	defer rows.Close()

	var released []ReleasedLock
	for rows.Next() {
		var r ReleasedLock
		if err := rows.Scan(&r.Key, &r.Owner); err != nil {
			return nil, fmt.Errorf("cluster: release stale: scan: %w", err)
		}
		released = append(released, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cluster: release stale: rows: %w", err)
	}

	if len(released) > 0 {
		// Drop any local entries whose lease was swept (they belong to this
		// process but were never refreshed in time).
		m.mu.Lock()
		for _, r := range released {
			if held, ok := m.held[r.Key]; ok && held.Owner == r.Owner {
				delete(m.held, r.Key)
			}
		}
		m.mu.Unlock()
	}
	return released, nil
}

// IsHeld reports whether the lock on key is currently held by this process.
// It does not query the database, so a lock held by another process returns
// false.
func (m *DistributedLockManager) IsHeld(key string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.held[key]
	return ok
}

// IsHeldBy reports whether the lock on key is held by the given owner in
// this process.
func (m *DistributedLockManager) IsHeldBy(key, owner string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lock, ok := m.held[key]
	return ok && lock.Owner == owner
}

// ListHeld returns a snapshot of all locks currently held by this process.
// The returned slice is safe to mutate.
func (m *DistributedLockManager) ListHeld() []DistributedLock {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]DistributedLock, 0, len(m.held))
	for _, l := range m.held {
		out = append(out, *l)
	}
	return out
}

// ReleaseAll frees every lock held by this process. It is called during
// cluster shutdown and ignores individual errors so that one stuck lock does
// not prevent the rest from being released.
func (m *DistributedLockManager) ReleaseAll(ctx context.Context) []error {
	m.mu.RLock()
	type heldRef struct {
		key   string
		owner string
	}
	refs := make([]heldRef, 0, len(m.held))
	for k, l := range m.held {
		refs = append(refs, heldRef{key: k, owner: l.Owner})
	}
	m.mu.RUnlock()

	var errs []error
	for _, r := range refs {
		if err := m.Release(ctx, r.key, r.owner); err != nil {
			errs = append(errs, fmt.Errorf("release %q: %w", r.key, err))
		}
	}
	return errs
}
