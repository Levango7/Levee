// distributed_lock_test.go exercises the DistributedLockManager. Tests that
// require a real PostgreSQL instance are skipped unless LEVEE_PG_TEST_DSN is
// set. Tests that only exercise the in-process state (IsHeld, ListHeld, etc.)
// run always.

package cluster

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgTestDSN returns the PostgreSQL DSN from the environment, or "" if unset.
func pgTestDSN() string {
	return os.Getenv("LEVEE_PG_TEST_DSN")
}

// TestDistributedLockManagerNilDB covers the no-database fallback paths.
func TestDistributedLockManagerNilDB(t *testing.T) {
	m := NewDistributedLockManager(nil)
	_, err := m.Acquire(context.Background(), "k", "o", time.Second)
	assert.Error(t, err)
	assert.False(t, m.IsHeld("k"))
	assert.False(t, m.IsHeldBy("k", "o"))
	assert.Empty(t, m.ListHeld())
}

// TestDistributedLockManagerValidation checks argument validation.
func TestDistributedLockManagerValidation(t *testing.T) {
	m := NewDistributedLockManager(nil)
	_, err := m.Acquire(context.Background(), "", "o", time.Second)
	assert.Error(t, err)
	_, err = m.Acquire(context.Background(), "k", "", time.Second)
	assert.Error(t, err)
}

// TestDistributedLockManagerReleaseNotHeld checks the error path.
func TestDistributedLockManagerReleaseNotHeld(t *testing.T) {
	m := NewDistributedLockManager(nil)
	err := m.Release(context.Background(), "k", "o")
	assert.True(t, errors.Is(err, ErrLockNotHeld))
}

// TestDistributedLockManagerRefreshNotHeld checks the error path.
func TestDistributedLockManagerRefreshNotHeld(t *testing.T) {
	m := NewDistributedLockManager(nil)
	err := m.Refresh(context.Background(), "k", "o", time.Second)
	assert.True(t, errors.Is(err, ErrLockNotHeld))
}

// TestLockKeyHashStable verifies that the hash is deterministic.
func TestLockKeyHashStable(t *testing.T) {
	assert.Equal(t, lockKeyHash("run:abc"), lockKeyHash("run:abc"))
	assert.NotEqual(t, lockKeyHash("run:abc"), lockKeyHash("run:abd"))
}

// TestDistributedLockAcquireRelease exercises the full acquire/release cycle
// against a real PostgreSQL instance. Skipped when no DSN is available.
func TestDistributedLockAcquireRelease(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL lock test")
	}

	db := openTestDB(t, dsn)
	defer db.Close()

	m := NewDistributedLockManager(db)
	ctx := context.Background()

	lock, err := m.Acquire(ctx, "test:lock:1", "owner-A", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "test:lock:1", lock.Key)
	assert.True(t, m.IsHeld("test:lock:1"))
	assert.True(t, m.IsHeldBy("test:lock:1", "owner-A"))

	// Re-acquire by same owner returns the same lock (fast path).
	lock2, err := m.Acquire(ctx, "test:lock:1", "owner-A", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, lock.Key, lock2.Key)

	// Acquire by a different owner fails with ErrLockBusy.
	_, err = m.Acquire(ctx, "test:lock:1", "owner-B", time.Minute)
	assert.True(t, errors.Is(err, ErrLockBusy))

	// Release by wrong owner fails.
	err = m.Release(ctx, "test:lock:1", "owner-B")
	assert.True(t, errors.Is(err, ErrLockNotHeld))

	// Release by correct owner succeeds.
	require.NoError(t, m.Release(ctx, "test:lock:1", "owner-A"))
	assert.False(t, m.IsHeld("test:lock:1"))

	// After release, another owner can acquire.
	_, err = m.Acquire(ctx, "test:lock:1", "owner-B", time.Minute)
	require.NoError(t, err)
	require.NoError(t, m.Release(ctx, "test:lock:1", "owner-B"))
}

// TestDistributedLockRefresh exercises the Refresh method.
func TestDistributedLockRefresh(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL lock test")
	}

	db := openTestDB(t, dsn)
	defer db.Close()

	m := NewDistributedLockManager(db)
	ctx := context.Background()

	_, err := m.Acquire(ctx, "test:lock:refresh", "owner", time.Minute)
	require.NoError(t, err)

	require.NoError(t, m.Refresh(ctx, "test:lock:refresh", "owner", 2*time.Minute))
	assert.True(t, m.IsHeldBy("test:lock:refresh", "owner"))

	require.NoError(t, m.Release(ctx, "test:lock:refresh", "owner"))
}

// TestDistributedLockReleaseAll exercises the bulk release path.
func TestDistributedLockReleaseAll(t *testing.T) {
	dsn := pgTestDSN()
	if dsn == "" {
		t.Skip("LEVEE_PG_TEST_DSN not set; skipping PostgreSQL lock test")
	}

	db := openTestDB(t, dsn)
	defer db.Close()

	m := NewDistributedLockManager(db)
	ctx := context.Background()

	_, err := m.Acquire(ctx, "test:lock:a1", "owner", time.Minute)
	require.NoError(t, err)
	_, err = m.Acquire(ctx, "test:lock:a2", "owner", time.Minute)
	require.NoError(t, err)
	assert.Len(t, m.ListHeld(), 2)

	errs := m.ReleaseAll(ctx)
	assert.Empty(t, errs)
	assert.Empty(t, m.ListHeld())
}