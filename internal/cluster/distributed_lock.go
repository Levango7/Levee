
// distributed_lock.go implements a distributed mutex backed by PostgreSQL
// advisory locks (pg_advisory_lock / pg_advisory_unlock).
//
// Advisory locks are session-level locks identified by a 64-bit integer key.
// They are not tied to any table or row, survive transactions, and are
// automatically released when the holding session disconnects. This makes
// them ideal for cluster-wide mutual exclusion in LEVEE: a master node holds
// the lock for a scope (e.g. "run:<id>") while orchestrating, and workers
// respect the lock before picking up work.
//
// We use the single-bigint form (pg_advisory_lock(bigint)) so the key space is
// the full int64 range. The lock key is derived from a stable hash of the
// caller-supplied string key (see lockKeyHash).
//
// Because advisory locks are session-scoped, the DistributedLockManager keeps
// a dedicated *sql.DB connection per held lock (acquired via db.Conn) so that
// releasing one lock does not release another held by the same process. The
// connection is returned to the pool on Release.

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// ErrLockNotHeld is returned when an operation requires the caller to hold a
// lock but the lock is not held by the given owner.
var ErrLockNotHeld = errors.New("cluster: lock not held")

// ErrLockBusy is returned when Acquire fails because another owner holds the
// lock. Callers should retry with backoff.
var ErrLockBusy = errors.New("cluster: lock busy")

// DistributedLock represents a held advisory lock. The fields are informational;
// the actual lock state lives in the PostgreSQL backend.
type DistributedLock struct {
	Key       string    // caller-supplied lock key
	Owner     string    // owner identifier (e.g. node ID)
	TTL       time.Duration
	AcquiredAt time.Time
	conn      *sql.Conn // dedicated connection holding the advisory lock
}

// DistributedLockManager coordinates PostgreSQL advisory locks. It is safe
// for concurrent use. A single manager should be shared by all goroutines in
// a process so that lock state (held connections) is centralised.
type DistributedLockManager struct {
	db *sql.DB

	mu    sync.RWMutex
	held  map[string]*DistributedLock // key -> lock
}

// NewDistributedLockManager returns a manager backed by the given *sql.DB.
// The db must point at the same PostgreSQL instance used by the PGStore.
func NewDistributedLockManager(db *sql.DB) *DistributedLockManager {
	if db == nil {
		return &DistributedLockManager{held: make(map[string]*DistributedLock)}
	}
	return &DistributedLockManager{db: db, held: make(map[string]*DistributedLock)}
}

// Acquire obtains an exclusive advisory lock on key for owner. If the lock is
// already held (by this process or another), it returns ErrLockBusy. The
// returned lock is tracked by the manager until Release is called.
//
// The ttl parameter is informational only — PostgreSQL advisory locks do not
// expire. Callers that want TTL semantics should run a background refresher
// (see Refresh) or use the database row-level lock pattern instead. We keep
// ttl in the struct so monitoring code can report stale acquisitions.
func (m *DistributedLockManager) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (*DistributedLock, error) {
	if key == "" {
		return nil, fmt.Errorf("cluster: acquire: empty key")
	}
	if owner == "" {
		return nil, fmt.Errorf("cluster: acquire: empty owner")
	}
	if m.db == nil {
		return nil, fmt.Errorf("cluster: acquire: nil db")
	}

	// Fast path: already held by this process.
	m.mu.RLock()
	if existing, ok := m.held[key]; ok {
		m.mu.RUnlock()
		if existing.Owner == owner {
			return existing, nil
		}
		return nil, ErrLockBusy
	}
	m.mu.RUnlock()

	// Slow path: try to acquire via pg_advisory_lock. We use a dedicated
	// connection so releasing this lock does not release others.
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("cluster: acquire conn: %w", err)
	}

	pgKey := lockKeyHash(key)
	// pg_try_advisory_lock returns true/false immediately; we use it to avoid
	// blocking on a lock held by another session.
	var ok bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", pgKey).Scan(&ok); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("cluster: try advisory lock: %w", err)
	}
	if !ok {
		_ = conn.Close()
		return nil, ErrLockBusy
	}

	lock := &DistributedLock{
		Key:        key,
		Owner:      owner,
		TTL:        ttl,
		AcquiredAt: time.Now().UTC(),
		conn:       conn,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check under write lock: another goroutine may have acquired the same
	// key between the fast path and here.
	if existing, ok := m.held[key]; ok {
		// Release our freshly acquired lock and return the existing one.
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", pgKey)
		_ = conn.Close()
		if existing.Owner == owner {
			return existing, nil
		}
		return nil, ErrLockBusy
	}
	m.held[key] = lock
	log.Debug("distributed lock acquired", "key", key, "owner", owner)
	return lock, nil
}

// Release frees the advisory lock held by owner on key. If the lock is not
// held by owner, it returns ErrLockNotHeld.
func (m *DistributedLockManager) Release(ctx context.Context, key, owner string) error {
	if key == "" {
		return fmt.Errorf("cluster: release: empty key")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.held[key]
	if !ok {
		return ErrLockNotHeld
	}
	if lock.Owner != owner {
		return ErrLockNotHeld
	}

	pgKey := lockKeyHash(key)
	var unlocked bool
	if err := lock.conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", pgKey).Scan(&unlocked); err != nil {
		// Even if the unlock query fails, drop the local state so the manager
		// does not leak entries. The connection close will release the lock
		// server-side anyway.
		log.Warn("advisory unlock query failed", "key", key, "error", err)
	}
	if !unlocked {
		log.Warn("advisory unlock returned false", "key", key)
	}
	if err := lock.conn.Close(); err != nil {
		log.Warn("lock conn close failed", "key", key, "error", err)
	}
	delete(m.held, key)
	log.Debug("distributed lock released", "key", key, "owner", owner)
	return nil
}

// Refresh extends the recorded TTL of a held lock. Because PostgreSQL advisory
// locks do not expire, this is a no-op apart from updating the in-memory
// AcquiredAt timestamp. It returns ErrLockNotHeld if the lock is not held by
// owner.
func (m *DistributedLockManager) Refresh(ctx context.Context, key, owner string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	lock, ok := m.held[key]
	if !ok || lock.Owner != owner {
		return ErrLockNotHeld
	}
	lock.AcquiredAt = time.Now().UTC()
	lock.TTL = ttl
	return nil
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

// IsHeldBy reports whether the lock on key is held by the given owner.
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
	m.mu.Lock()
	keys := make([]string, 0, len(m.held))
	for k := range m.held {
		keys = append(keys, k)
	}
	m.mu.Unlock()

	var errs []error
	for _, k := range keys {
		// We do not know the owner here, so we read it under the lock.
		m.mu.RLock()
		lock, ok := m.held[k]
		m.mu.RUnlock()
		if !ok {
			continue
		}
		if err := m.Release(ctx, k, lock.Owner); err != nil {
			errs = append(errs, fmt.Errorf("release %q: %w", k, err))
		}
	}
	return errs
}

// lockKeyHash derives a stable int64 PostgreSQL advisory-lock key from a
// string key. We use FNV-1a because it is fast, has good distribution for
// short strings, and the int64 result fits pg_advisory_lock(bigint).
//
// The hash is deterministic across processes (unlike a Go map hash), so two
// processes acquiring "run:abc" will hit the same PostgreSQL lock key.
func lockKeyHash(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	// Convert uint64 to int64. PostgreSQL advisory lock accepts bigint, so
	// negative values are fine; we just need a 1:1 mapping.
	return int64(h.Sum64())
}