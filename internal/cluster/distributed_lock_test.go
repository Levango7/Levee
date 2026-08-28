// distributed_lock_test.go exercises the DistributedLockManager. Tests that
// require a real PostgreSQL instance are skipped unless LEVEE_PG_TEST_DSN is
// set. Tests that only exercise argument validation and the nil-db fallback
// run always.

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgTestDSN returns the PostgreSQL DSN from the environment, or "" if unset.
func pgTestDSN() string {
	return os.Getenv("LEVEE_PG_TEST_DSN")
}

// newTestLockManager opens the test PostgreSQL instance, ensures the cluster
// schema and returns a lock manager backed by it. The database handle is
// closed via t.Cleanup. Skips the test when no DSN is configured.
func newTestLockManager(t *testing.T) (*DistributedLockManager, *sql.DB) {
	t.Helper()
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL lock test")
	}
	db := openTestDB(t, dsn)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, ensureClusterSchema(context.Background(), db))
	return NewDistributedLockManager(db), db
}

// TestDistributedLockManagerNilDB covers the no-database fallback paths.
func TestDistributedLockManagerNilDB(t *testing.T) {
	m := NewDistributedLockManager(nil)
	_, err := m.Acquire(context.Background(), "k", "o", time.Second)
	assert.Error(t, err)
	assert.Error(t, m.Release(context.Background(), "k", "o"))
	assert.Error(t, m.Refresh(context.Background(), "k", "o", time.Second))
	_, err = m.ReleaseStale(context.Background())
	assert.Error(t, err)
	assert.False(t, m.IsHeld("k"))
	assert.False(t, m.IsHeldBy("k", "o"))
	assert.Empty(t, m.ListHeld())
}

// TestDistributedLockManagerValidation checks argument validation.
func TestDistributedLockManagerValidation(t *testing.T) {
	m := NewDistributedLockManager(nil)
	ctx := context.Background()
	_, err := m.Acquire(ctx, "", "o", time.Second)
	assert.Error(t, err)
	_, err = m.Acquire(ctx, "k", "", time.Second)
	assert.Error(t, err)
	_, err = m.Acquire(ctx, "k", "o", 0)
	assert.Error(t, err)
	assert.Error(t, m.Refresh(ctx, "k", "o", 0))
}

// TestDistributedLockAcquireRelease exercises the full acquire/release cycle
// against a real PostgreSQL instance.
func TestDistributedLockAcquireRelease(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	key := fmt.Sprintf("test:lock:cycle:%d", time.Now().UnixNano())

	lock, err := m.Acquire(ctx, key, "owner-A", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, key, lock.Key)
	assert.NotZero(t, lock.FenceToken)
	assert.True(t, m.IsHeld(key))
	assert.True(t, m.IsHeldBy(key, "owner-A"))

	// Acquire by a different owner fails with ErrLockBusy while the lease is live.
	_, err = m.Acquire(ctx, key, "owner-B", time.Minute)
	assert.True(t, errors.Is(err, ErrLockBusy))

	// Release by wrong owner fails.
	err = m.Release(ctx, key, "owner-B")
	assert.True(t, errors.Is(err, ErrLockNotHeld))

	// Release by correct owner succeeds.
	require.NoError(t, m.Release(ctx, key, "owner-A"))
	assert.False(t, m.IsHeld(key))

	// After release, another owner can acquire — with a fresh fence token.
	lock2, err := m.Acquire(ctx, key, "owner-B", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, lock2.FenceToken, lock.FenceToken)
	require.NoError(t, m.Release(ctx, key, "owner-B"))
}

// TestDistributedLockReacquireSameOwnerKeepsFence verifies that re-acquiring
// a key already owned refreshes the lease without bumping the fence token.
func TestDistributedLockReacquireSameOwnerKeepsFence(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	key := fmt.Sprintf("test:lock:reacquire:%d", time.Now().UnixNano())

	lock, err := m.Acquire(ctx, key, "owner", time.Minute)
	require.NoError(t, err)

	lock2, err := m.Acquire(ctx, key, "owner", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, lock.FenceToken, lock2.FenceToken)

	require.NoError(t, m.Release(ctx, key, "owner"))
}

// TestDistributedLockReclaimAfterExpiry is the core lease property: once the
// lease expires (holder crashed or lost connectivity), another owner can
// take the lock over without any intervention.
func TestDistributedLockReclaimAfterExpiry(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	key := fmt.Sprintf("test:lock:reclaim:%d", time.Now().UnixNano())

	lock, err := m.Acquire(ctx, key, "dead-node", time.Second)
	require.NoError(t, err)

	// While the lease is live the key is busy.
	_, err = m.Acquire(ctx, key, "successor", time.Minute)
	assert.True(t, errors.Is(err, ErrLockBusy))

	// Wait out the lease. No refresh happens, simulating a crashed holder.
	time.Sleep(1500 * time.Millisecond)

	lock2, err := m.Acquire(ctx, key, "successor", time.Minute)
	require.NoError(t, err)
	assert.Greater(t, lock2.FenceToken, lock.FenceToken)

	// The zombie's refresh now fails: its lease is gone.
	err = m.Refresh(ctx, key, "dead-node", time.Minute)
	assert.True(t, errors.Is(err, ErrLockNotHeld))

	require.NoError(t, m.Release(ctx, key, "successor"))
}

// TestDistributedLockRefreshExtendsLease verifies that a live holder that
// refreshes keeps the key past the original lease expiry.
func TestDistributedLockRefreshExtendsLease(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	key := fmt.Sprintf("test:lock:refresh:%d", time.Now().UnixNano())

	_, err := m.Acquire(ctx, key, "owner", time.Second)
	require.NoError(t, err)

	// Refresh well before expiry, extending the lease.
	require.NoError(t, m.Refresh(ctx, key, "owner", time.Minute))

	// Past the original 1s expiry the lease must still be live.
	time.Sleep(1500 * time.Millisecond)
	_, err = m.Acquire(ctx, key, "intruder", time.Minute)
	assert.True(t, errors.Is(err, ErrLockBusy))

	// Refresh by the wrong owner fails.
	err = m.Refresh(ctx, key, "intruder", time.Minute)
	assert.True(t, errors.Is(err, ErrLockNotHeld))

	require.NoError(t, m.Release(ctx, key, "owner"))
}

// TestDistributedLockReleaseStale verifies that ReleaseStale removes only
// expired lease rows.
func TestDistributedLockReleaseStale(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	staleKey := fmt.Sprintf("test:lock:stale:%d", time.Now().UnixNano())
	liveKey := fmt.Sprintf("test:lock:live:%d", time.Now().UnixNano())

	_, err := m.Acquire(ctx, staleKey, "dead", 500*time.Millisecond)
	require.NoError(t, err)
	_, err = m.Acquire(ctx, liveKey, "alive", time.Minute)
	require.NoError(t, err)

	time.Sleep(time.Second)

	released, err := m.ReleaseStale(ctx)
	require.NoError(t, err)

	var keys []string
	for _, r := range released {
		keys = append(keys, r.Key)
	}
	assert.Contains(t, keys, staleKey)
	assert.NotContains(t, keys, liveKey)

	// Idempotent: a second sweep does not report the same row again.
	released2, err := m.ReleaseStale(ctx)
	require.NoError(t, err)
	for _, r := range released2 {
		assert.NotEqual(t, staleKey, r.Key)
	}

	require.NoError(t, m.Release(ctx, liveKey, "alive"))
}

// TestDistributedLockConcurrentAcquire races several managers against one
// key and asserts exactly one wins while the lease is live.
func TestDistributedLockConcurrentAcquire(t *testing.T) {
	_, db := newTestLockManager(t)
	ctx := context.Background()
	key := fmt.Sprintf("test:lock:race:%d", time.Now().UnixNano())

	const contenders = 8
	var wg sync.WaitGroup
	wins := make(chan string, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			m := NewDistributedLockManager(db)
			owner := fmt.Sprintf("racer-%d", id)
			if _, err := m.Acquire(ctx, key, owner, time.Minute); err == nil {
				wins <- owner
			} else {
				assert.True(t, errors.Is(err, ErrLockBusy), "unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	close(wins)

	var winners []string
	for w := range wins {
		winners = append(winners, w)
	}
	require.Len(t, winners, 1, "exactly one contender must win the lease")

	// Cleanup via a fresh manager owned by the winner.
	cleanup := NewDistributedLockManager(db)
	require.NoError(t, cleanup.Release(ctx, key, winners[0]))
}

// TestDistributedLockReleaseAll exercises the bulk release path.
func TestDistributedLockReleaseAll(t *testing.T) {
	m, _ := newTestLockManager(t)
	ctx := context.Background()
	k1 := fmt.Sprintf("test:lock:all1:%d", time.Now().UnixNano())
	k2 := fmt.Sprintf("test:lock:all2:%d", time.Now().UnixNano())

	_, err := m.Acquire(ctx, k1, "owner", time.Minute)
	require.NoError(t, err)
	_, err = m.Acquire(ctx, k2, "owner", time.Minute)
	require.NoError(t, err)
	assert.Len(t, m.ListHeld(), 2)

	errs := m.ReleaseAll(ctx)
	assert.Empty(t, errs)
	assert.Empty(t, m.ListHeld())
}
