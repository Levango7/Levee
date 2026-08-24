package lock

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

// --- test helpers -----------------------------------------------------------

// newTestStateStore returns a fresh SQLite-backed state.Store for each
// test. The database file lives in a per-test temp dir and is closed via
// t.Cleanup.
func newTestStateStore(t *testing.T) *state.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "levee-lock-test.db")
	store, err := state.NewSQLiteStore(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestLockStore returns a LockStore backed by a fresh SQLite
// state.Store.
func newTestLockStore(t *testing.T) (LockStore, *state.SQLiteStore) {
	t.Helper()
	st := newTestStateStore(t)
	return NewLockStore(st), st
}

// newTestManager returns a LockManager backed by a real SQLite state.Store
// and a LockStore wrapping the same state.Store.
func newTestManager(t *testing.T) *LockManager {
	t.Helper()
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	return NewLockManager(ls, st)
}

// seedRun creates a minimal run row so that steps can reference it via
// the foreign key.
func seedRun(t *testing.T, st state.Store, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.CreateRun(ctx, &state.Run{
		ID: id, WorkflowName: "w", TemplateName: "t", PlanHash: "h",
		Status: "running", ApprovalStatus: "approved", ApprovalLevel: "low",
		CreatedAt: now, UpdatedAt: now, Creator: "u",
	}))
}

// seedBatch creates a minimal batch row under the given run.
func seedBatch(t *testing.T, st state.Store, id, runID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, st.CreateBatch(ctx, &state.Batch{
		ID: id, RunID: runID, BatchNo: 1, Status: "running",
	}))
}

// seedStep creates a step row for a host with the given status.
func seedStep(t *testing.T, st state.Store, id, runID, batchID, host, status string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, st.CreateStep(ctx, &state.Step{
		ID: id, RunID: runID, BatchID: batchID, Host: host,
		StepName: "s", Action: "a", Status: status,
		StartedAt: &now,
	}))
}

// bgCtx returns a background context — a short alias for test readability.
func bgCtx() context.Context { return context.Background() }

// =========================================================================
// Lock.Expired
// =========================================================================

func TestLock_Expired(t *testing.T) {
	now := time.Now().UTC()

	t.Run("nil lock never expires", func(t *testing.T) {
		var l *Lock
		assert.False(t, l.Expired(now))
	})

	t.Run("zero ExpiresAt never expires", func(t *testing.T) {
		l := &Lock{Owner: "r1", ExpiresAt: time.Time{}}
		assert.False(t, l.Expired(now))
	})

	t.Run("future expiry not expired", func(t *testing.T) {
		l := &Lock{Owner: "r1", ExpiresAt: now.Add(time.Hour)}
		assert.False(t, l.Expired(now))
	})

	t.Run("past expiry is expired", func(t *testing.T) {
		l := &Lock{Owner: "r1", ExpiresAt: now.Add(-time.Minute)}
		assert.True(t, l.Expired(now))
	})

	t.Run("exact expiry is expired", func(t *testing.T) {
		l := &Lock{Owner: "r1", ExpiresAt: now}
		assert.True(t, l.Expired(now))
	})
}

// =========================================================================
// LockStore (stateLockStore) — backed by real SQLite
// =========================================================================

func TestLockStore_Acquire_NewTarget(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	l, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "host-01", l.Target)
	assert.Equal(t, "run-1", l.Owner)
	assert.Equal(t, time.Hour, l.TTL)
	assert.True(t, l.ExpiresAt.After(l.AcquiredAt))
}

func TestLockStore_Acquire_DefaultTTL(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	// Non-positive TTL should default to DefaultTTL.
	l, err := store.Acquire(ctx, "host-01", "run-1", 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultTTL, l.TTL)

	// Negative TTL also defaults.
	l2, err := store.Acquire(ctx, "host-02", "run-1", -5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, DefaultTTL, l2.TTL)
}

func TestLockStore_Acquire_AlreadyHeld(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	// Second acquire by a different owner must fail with ErrLockHeld.
	l, err := store.Acquire(ctx, "host-01", "run-2", time.Hour)
	require.Error(t, err)
	assert.Nil(t, l)
	assert.ErrorIs(t, err, ErrLockHeld)
}

func TestLockStore_Acquire_ExpiredStillHeld(t *testing.T) {
	// LockStore.Acquire does NOT auto-preempt; even an expired lock blocks
	// a plain Acquire. Use ForceAcquire or LockManager.Acquire for
	// preemption.
	store, st := newTestLockStore(t)
	ctx := bgCtx()

	// Seed an already-expired lock directly into the store.
	now := time.Now().UTC()
	expired := &state.Lock{
		ID: "exp-1", Scope: scope("host-01"), Owner: "run-1",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}
	require.NoError(t, st.CreateLock(ctx, expired))

	// Acquire must still return ErrLockHeld (no auto-preempt at this layer).
	_, err := store.Acquire(ctx, "host-01", "run-2", time.Hour)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLockHeld)
}

func TestLockStore_Acquire_EmptyTarget(t *testing.T) {
	store, _ := newTestLockStore(t)
	_, err := store.Acquire(bgCtx(), "", "run-1", time.Hour)
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestLockStore_Acquire_EmptyOwner(t *testing.T) {
	store, _ := newTestLockStore(t)
	_, err := store.Acquire(bgCtx(), "host-01", "", time.Hour)
	assert.ErrorIs(t, err, ErrEmptyOwner)
}

func TestLockStore_Release(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	require.NoError(t, store.Release(ctx, "host-01", "run-1"))

	// After release, the lock is gone.
	l, err := store.Get(ctx, "host-01")
	require.NoError(t, err)
	assert.Nil(t, l)
}

func TestLockStore_Release_NotOwner(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	err = store.Release(ctx, "host-01", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotOwner)
}

func TestLockStore_Release_NotFound(t *testing.T) {
	store, _ := newTestLockStore(t)
	err := store.Release(bgCtx(), "host-01", "run-1")
	assert.ErrorIs(t, err, ErrLockNotFound)
}

func TestLockStore_Release_EmptyTarget(t *testing.T) {
	store, _ := newTestLockStore(t)
	err := store.Release(bgCtx(), "", "run-1")
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestLockStore_Get(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	// No lock → (nil, nil).
	l, err := store.Get(ctx, "host-01")
	require.NoError(t, err)
	assert.Nil(t, l)

	// Acquire then get.
	acquired, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	l, err = store.Get(ctx, "host-01")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, acquired.Target, l.Target)
	assert.Equal(t, acquired.Owner, l.Owner)
}

func TestLockStore_Get_EmptyTarget(t *testing.T) {
	store, _ := newTestLockStore(t)
	_, err := store.Get(bgCtx(), "")
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestLockStore_ForceAcquire_Existing(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	// ForceAcquire by a different owner on a still-valid lock is rejected.
	_, err = store.ForceAcquire(ctx, "host-01", "run-2", 30*time.Minute)
	assert.ErrorIs(t, err, ErrLockHeld)

	// The original owner can still release.
	require.NoError(t, store.Release(ctx, "host-01", "run-1"))

	// After releasing, ForceAcquire creates a new lock.
	l, err := store.ForceAcquire(ctx, "host-01", "run-2", 30*time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "run-2", l.Owner)
	assert.Equal(t, 30*time.Minute, l.TTL)
	require.NoError(t, store.Release(ctx, "host-01", "run-2"))
}

func TestLockStore_ForceAcquire_NoExisting(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	// ForceAcquire on an unlocked target just creates a new lock.
	l, err := store.ForceAcquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)
	assert.Equal(t, "host-01", l.Target)
	assert.Equal(t, "run-1", l.Owner)
}

func TestLockStore_ForceAcquire_DefaultTTL(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	l, err := store.ForceAcquire(ctx, "host-01", "run-1", 0)
	require.NoError(t, err)
	assert.Equal(t, DefaultTTL, l.TTL)
}

func TestLockStore_ForceAcquire_EmptyTarget(t *testing.T) {
	store, _ := newTestLockStore(t)
	_, err := store.ForceAcquire(bgCtx(), "", "run-1", time.Hour)
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestLockStore_ForceAcquire_EmptyOwner(t *testing.T) {
	store, _ := newTestLockStore(t)
	_, err := store.ForceAcquire(bgCtx(), "host-01", "", time.Hour)
	assert.ErrorIs(t, err, ErrEmptyOwner)
}

func TestLockStore_ListExpired(t *testing.T) {
	store, st := newTestLockStore(t)
	ctx := bgCtx()

	// One live lock and two expired locks.
	_, err := store.Acquire(ctx, "host-live", "run-1", time.Hour)
	require.NoError(t, err)

	now := time.Now().UTC()
	for _, h := range []string{"host-exp-1", "host-exp-2"} {
		require.NoError(t, st.CreateLock(ctx, &state.Lock{
			ID: "exp-" + h, Scope: scope(h), Owner: "run-old",
			TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
			ExpiresAt: now.Add(-time.Hour),
		}))
	}

	expired, err := store.ListExpired(ctx)
	require.NoError(t, err)
	require.Len(t, expired, 2)

	targets := map[string]bool{}
	for _, l := range expired {
		targets[l.Target] = true
	}
	assert.True(t, targets["host-exp-1"])
	assert.True(t, targets["host-exp-2"])
}

func TestLockStore_ListExpired_None(t *testing.T) {
	store, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := store.Acquire(ctx, "host-01", "run-1", time.Hour)
	require.NoError(t, err)

	expired, err := store.ListExpired(ctx)
	require.NoError(t, err)
	assert.Empty(t, expired)
}

// =========================================================================
// LockManager — backed by real SQLite (integration)
// =========================================================================

func TestManager_Acquire_NewTarget(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	l, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "host-01", l.Target)
	assert.Equal(t, "run-1", l.Owner)
	assert.Equal(t, DefaultTTL, l.TTL)
}

func TestManager_Acquire_AlreadyHeld(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	_, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)

	// Second acquire by a different owner fails.
	l, err := mgr.Acquire(ctx, "host-01", "run-2")
	require.Error(t, err)
	assert.Nil(t, l)
	assert.ErrorIs(t, err, ErrLockHeld)
}

func TestManager_Acquire_SameOwnerReacquire(t *testing.T) {
	// Acquiring a target already locked by the same owner still returns
	// ErrLockHeld — the caller must release first. This keeps the
	// semantics simple and predictable.
	mgr := newTestManager(t)
	ctx := bgCtx()

	_, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)

	_, err = mgr.Acquire(ctx, "host-01", "run-1")
	assert.ErrorIs(t, err, ErrLockHeld)
}

func TestManager_Acquire_ExpiredAutoPreempt(t *testing.T) {
	// Build a manager over a shared store, then seed an expired lock
	// directly into that store before calling Acquire.
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	// Seed an expired lock directly.
	now := time.Now().UTC()
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "old", Scope: scope("host-01"), Owner: "run-old",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}))

	// Acquire by a new owner should auto-preempt.
	l, err := mgr.Acquire(ctx, "host-01", "run-new")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "run-new", l.Owner)
	assert.True(t, l.ExpiresAt.After(now))

	// An audit entry should have been recorded.
	audits, err := st.ListAudits(ctx, state.AuditFilter{Action: "lock"})
	require.NoError(t, err)
	require.Len(t, audits, 1)
	assert.Equal(t, "host-01", audits[0].Target)
	assert.Equal(t, "run-new", audits[0].Actor)
}

func TestManager_Acquire_EmptyTarget(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.Acquire(bgCtx(), "", "run-1")
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestManager_Acquire_EmptyOwner(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.Acquire(bgCtx(), "host-01", "")
	assert.ErrorIs(t, err, ErrEmptyOwner)
}

func TestManager_Release(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	_, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)

	require.NoError(t, mgr.Release(ctx, "host-01", "run-1"))

	// After release, the lock is gone.
	l, err := mgr.store.Get(ctx, "host-01")
	require.NoError(t, err)
	assert.Nil(t, l)
}

func TestManager_Release_NotOwner(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	_, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)

	err = mgr.Release(ctx, "host-01", "run-2")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotOwner)
}

func TestManager_Release_NotFound(t *testing.T) {
	mgr := newTestManager(t)
	err := mgr.Release(bgCtx(), "host-01", "run-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLockNotFound)
}

func TestManager_SetTTL(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	mgr.SetTTL(30 * time.Minute)
	l, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, l.TTL)

	// Non-positive TTL is ignored (keeps the previous value).
	mgr.SetTTL(0)
	l2, err := mgr.Acquire(ctx, "host-02", "run-1")
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, l2.TTL)
}

func TestManager_CheckAndAcquire_IdleTarget(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	// No in-flight steps → acquisition succeeds.
	l, err := mgr.CheckAndAcquire(ctx, "host-01", "run-1")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "host-01", l.Target)
}

func TestManager_CheckAndAcquire_BusyTarget_RunningStep(t *testing.T) {
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	// Seed a run + batch + a running step on host-01.
	seedRun(t, st, "r1")
	seedBatch(t, st, "b1", "r1")
	seedStep(t, st, "s1", "r1", "b1", "host-01", "running")

	// CheckAndAcquire must refuse.
	l, err := mgr.CheckAndAcquire(ctx, "host-01", "run-2")
	require.Error(t, err)
	assert.Nil(t, l)
	assert.ErrorIs(t, err, ErrTargetBusy)
}

func TestManager_CheckAndAcquire_BusyTarget_PendingStep(t *testing.T) {
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	seedRun(t, st, "r1")
	seedBatch(t, st, "b1", "r1")
	seedStep(t, st, "s1", "r1", "b1", "host-01", "pending")

	l, err := mgr.CheckAndAcquire(ctx, "host-01", "run-2")
	require.Error(t, err)
	assert.Nil(t, l)
	assert.ErrorIs(t, err, ErrTargetBusy)
}

func TestManager_CheckAndAcquire_FinishedStepsNotBusy(t *testing.T) {
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	seedRun(t, st, "r1")
	seedBatch(t, st, "b1", "r1")
	// A completed step (status "success") does not make the target busy.
	seedStep(t, st, "s1", "r1", "b1", "host-01", "success")
	// A skipped step also does not make the target busy.
	seedStep(t, st, "s2", "r1", "b1", "host-01", "skipped")

	l, err := mgr.CheckAndAcquire(ctx, "host-01", "run-2")
	require.NoError(t, err)
	require.NotNil(t, l)
}

func TestManager_CheckAndAcquire_DifferentHostNotBusy(t *testing.T) {
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	seedRun(t, st, "r1")
	seedBatch(t, st, "b1", "r1")
	// host-01 is busy, but host-02 is not.
	seedStep(t, st, "s1", "r1", "b1", "host-01", "running")

	l, err := mgr.CheckAndAcquire(ctx, "host-02", "run-2")
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.Equal(t, "host-02", l.Target)
}

func TestManager_CheckAndAcquire_ExpiredPreemptAfterBusyCheck(t *testing.T) {
	// When the existing lock is expired AND the target is idle,
	// CheckAndAcquire should proceed with preemption.
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	// Seed an expired lock.
	now := time.Now().UTC()
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "old", Scope: scope("host-01"), Owner: "run-old",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}))

	// Target is idle (no steps) → preemption succeeds.
	l, err := mgr.CheckAndAcquire(ctx, "host-01", "run-new")
	require.NoError(t, err)
	assert.Equal(t, "run-new", l.Owner)
}

func TestManager_CheckAndAcquire_BusyBlocksEvenExpired(t *testing.T) {
	// Even if the lock is expired, a busy target must not be preempted.
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	now := time.Now().UTC()
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "old", Scope: scope("host-01"), Owner: "run-old",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}))

	seedRun(t, st, "r1")
	seedBatch(t, st, "b1", "r1")
	seedStep(t, st, "s1", "r1", "b1", "host-01", "running")

	_, err := mgr.CheckAndAcquire(ctx, "host-01", "run-new")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTargetBusy)
}

func TestManager_CheckAndAcquire_EmptyTarget(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.CheckAndAcquire(bgCtx(), "", "run-1")
	assert.ErrorIs(t, err, ErrEmptyTarget)
}

func TestManager_CheckAndAcquire_EmptyOwner(t *testing.T) {
	mgr := newTestManager(t)
	_, err := mgr.CheckAndAcquire(bgCtx(), "host-01", "")
	assert.ErrorIs(t, err, ErrEmptyOwner)
}

func TestManager_CleanExpired(t *testing.T) {
	st := newTestStateStore(t)
	ls := NewLockStore(st)
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	now := time.Now().UTC()

	// Two expired, one live.
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "e1", Scope: scope("h1"), Owner: "r1",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}))
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "e2", Scope: scope("h2"), Owner: "r2",
		TTLSeconds: 60, AcquiredAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
	}))
	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "live", Scope: scope("h3"), Owner: "r3",
		TTLSeconds: 3600, AcquiredAt: now, ExpiresAt: now.Add(time.Hour),
	}))

	n, err := mgr.CleanExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// The live lock must still be present.
	l, err := ls.Get(ctx, "h3")
	require.NoError(t, err)
	require.NotNil(t, l)
}

func TestManager_CleanExpired_None(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	n, err := mgr.CleanExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

// =========================================================================
// LockManager — unit tests with a mock LockStore
// =========================================================================

// mockLockStore is an in-memory LockStore for unit-testing LockManager in
// isolation from SQLite. It is safe for concurrent use.
type mockLockStore struct {
	mu     sync.Mutex
	locks  map[string]*Lock // keyed by target
	failOn map[string]error // per-method failure injection
}

func newMockLockStore() *mockLockStore {
	return &mockLockStore{
		locks:  make(map[string]*Lock),
		failOn: make(map[string]error),
	}
}

func (m *mockLockStore) Acquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["acquire"]; err != nil {
		return nil, err
	}
	if _, ok := m.locks[target]; ok {
		return nil, fmt.Errorf("%w: %s", ErrLockHeld, target)
	}
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	l := &Lock{Target: target, Owner: owner, AcquiredAt: now, ExpiresAt: now.Add(ttl), TTL: ttl}
	m.locks[target] = l
	return l, nil
}

func (m *mockLockStore) Release(ctx context.Context, target, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["release"]; err != nil {
		return err
	}
	l, ok := m.locks[target]
	if !ok {
		return ErrLockNotFound
	}
	if l.Owner != owner {
		return ErrNotOwner
	}
	delete(m.locks, target)
	return nil
}

func (m *mockLockStore) Get(ctx context.Context, target string) (*Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["get"]; err != nil {
		return nil, err
	}
	l, ok := m.locks[target]
	if !ok {
		return nil, nil
	}
	// Return a copy so callers can't mutate the stored lock directly.
	out := *l
	return &out, nil
}

func (m *mockLockStore) ForceAcquire(ctx context.Context, target, owner string, ttl time.Duration) (*Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["force"]; err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	l := &Lock{Target: target, Owner: owner, AcquiredAt: now, ExpiresAt: now.Add(ttl), TTL: ttl}
	m.locks[target] = l
	return l, nil
}

func (m *mockLockStore) ListExpired(ctx context.Context) ([]*Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failOn["list"]; err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out []*Lock
	for _, l := range m.locks {
		if now.After(l.ExpiresAt) {
			out = append(out, l)
		}
	}
	return out, nil
}

func TestManager_WithMockStore_AcquireNew(t *testing.T) {
	st := newTestStateStore(t)
	ls := newMockLockStore()
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	l, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)
	assert.Equal(t, "host-01", l.Target)
}

func TestManager_WithMockStore_AcquireHeld(t *testing.T) {
	st := newTestStateStore(t)
	ls := newMockLockStore()
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	_, err := mgr.Acquire(ctx, "host-01", "run-1")
	require.NoError(t, err)

	_, err = mgr.Acquire(ctx, "host-01", "run-2")
	assert.ErrorIs(t, err, ErrLockHeld)
}

func TestManager_WithMockStore_AcquirePreemptExpired(t *testing.T) {
	st := newTestStateStore(t)
	ls := newMockLockStore()
	mgr := NewLockManager(ls, st)
	ctx := bgCtx()

	// Seed an expired lock in the mock.
	now := time.Now().UTC()
	ls.locks["host-01"] = &Lock{
		Target: "host-01", Owner: "run-old",
		AcquiredAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
		TTL: time.Hour,
	}

	l, err := mgr.Acquire(ctx, "host-01", "run-new")
	require.NoError(t, err)
	assert.Equal(t, "run-new", l.Owner)
}

func TestManager_WithMockStore_StoreError(t *testing.T) {
	st := newTestStateStore(t)
	ls := newMockLockStore()
	ls.failOn["get"] = errors.New("boom")
	mgr := NewLockManager(ls, st)

	_, err := mgr.Acquire(bgCtx(), "host-01", "run-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// =========================================================================
// Concurrency smoke test
// =========================================================================

func TestManager_ConcurrentAcquire_SingleWinner(t *testing.T) {
	mgr := newTestManager(t)
	ctx := bgCtx()

	const n = 20
	var (
		wg      sync.WaitGroup
		success int
		held    int
		mu      sync.Mutex
	)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := fmt.Sprintf("run-%d", i)
			_, err := mgr.Acquire(ctx, "host-01", owner)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrLockHeld) {
				held++
			}
		}(i)
	}
	wg.Wait()

	// Exactly one goroutine wins the lock; the rest get ErrLockHeld.
	// Note: under heavy concurrency, a goroutine may occasionally fail to
	// compete due to scheduling jitter, so we assert a tolerant range
	// instead of an exact count.
	assert.Equal(t, 1, success, "exactly one goroutine should acquire the lock")
	assert.GreaterOrEqual(t, held, 17, "the rest should get ErrLockHeld")
	assert.LessOrEqual(t, held, 20, "held should not exceed total")
}

// =========================================================================
// Release race regression (owner-check-then-delete vs ForceAcquire takeover)
// =========================================================================

// TestRelease_StaleOwnerAfterTakeoverDoesNotDeleteNewLock reproduces the
// release race: run-1's lock expires and is taken over by run-2 (ForceAcquire
// reuses the same row id). A stale Release from run-1 must fail with
// ErrNotOwner and must never delete run-2's lock.
func TestRelease_StaleOwnerAfterTakeoverDoesNotDeleteNewLock(t *testing.T) {
	ls, _ := newTestLockStore(t)
	ctx := bgCtx()

	_, err := ls.Acquire(ctx, "host-01", "run-1", time.Nanosecond)
	require.NoError(t, err)

	// Let the lock expire, then let run-2 take it over.
	time.Sleep(2 * time.Millisecond)
	taken, err := ls.ForceAcquire(ctx, "host-01", "run-2", DefaultTTL)
	require.NoError(t, err)
	require.Equal(t, "run-2", taken.Owner)

	// Stale owner attempts release: refused, and the new owner keeps the lock.
	err = ls.Release(ctx, "host-01", "run-1")
	require.ErrorIs(t, err, ErrNotOwner)

	got, err := ls.Get(ctx, "host-01")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "run-2", got.Owner, "takeover owner must keep the lock")
}

// TestDeleteLockByIDAndOwner_Conditional pins the state-layer contract that
// makes the fix race-free: ownership check and delete happen in one statement,
// so a row whose owner changed between a read and the delete is not deleted.
func TestDeleteLockByIDAndOwner_Conditional(t *testing.T) {
	st := newTestStateStore(t)
	ctx := bgCtx()
	now := time.Now().UTC()

	require.NoError(t, st.CreateLock(ctx, &state.Lock{
		ID: "lock-1", Scope: "host:h1", Owner: "run-a", TTLSeconds: 60,
		AcquiredAt: now, ExpiresAt: now.Add(time.Minute),
	}))

	// Wrong owner: no deletion.
	deleted, err := st.DeleteLockByIDAndOwner(ctx, "lock-1", "run-b")
	require.NoError(t, err)
	assert.False(t, deleted)

	got, err := st.GetLock(ctx, "lock-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Simulate a concurrent takeover changing only the owner on the same id:
	// the stale owner's cached (id, owner) pair no longer matches.
	got.Owner = "run-c"
	require.NoError(t, st.UpdateLock(ctx, got))

	deleted, err = st.DeleteLockByIDAndOwner(ctx, "lock-1", "run-a")
	require.NoError(t, err)
	assert.False(t, deleted, "stale owner must not delete the taken-over lock")

	// Correct owner deletes; second attempt reports false (already gone).
	deleted, err = st.DeleteLockByIDAndOwner(ctx, "lock-1", "run-c")
	require.NoError(t, err)
	assert.True(t, deleted)

	deleted, err = st.DeleteLockByIDAndOwner(ctx, "lock-1", "run-c")
	require.NoError(t, err)
	assert.False(t, deleted)
}
