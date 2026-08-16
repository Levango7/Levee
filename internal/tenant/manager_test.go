package tenant

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTenantManager(t *testing.T) {
	tm := NewTenantManager()
	assert.NotNil(t, tm)
	assert.NotNil(t, tm.QuotaManager())
	assert.Equal(t, 0, tm.Count())
}

func TestTenantManagerCreate(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME Corp", Quota{MaxTargets: 10})
	require.NoError(t, err)
	assert.Equal(t, "acme", tt.Name)
	assert.Equal(t, TenantActive, tt.Status)
	assert.Equal(t, 1, tm.Count())

	// Quota should be installed.
	q, err := tm.QuotaManager().GetQuota(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, 10, q.MaxTargets)
}

func TestTenantManagerCreateDuplicateName(t *testing.T) {
	tm := NewTenantManager()
	_, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	_, err = tm.Create(context.Background(), "acme", "ACME 2", Quota{})
	assert.ErrorIs(t, err, ErrTenantExists)
}

func TestTenantManagerCreateInvalidName(t *testing.T) {
	tm := NewTenantManager()
	_, err := tm.Create(context.Background(), "ACME", "ACME", Quota{})
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTenant)
}

func TestTenantManagerCreateEmptyName(t *testing.T) {
	tm := NewTenantManager()
	_, err := tm.Create(context.Background(), "", "ACME", Quota{})
	assert.Error(t, err)
}

func TestTenantManagerCreateInvalidQuota(t *testing.T) {
	tm := NewTenantManager()
	_, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: -1})
	assert.Error(t, err)
}

func TestTenantManagerGet(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, tt.ID, got.ID)

	_, err = tm.Get("missing")
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerGetByName(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	got, err := tm.GetByName("acme")
	require.NoError(t, err)
	assert.Equal(t, tt.ID, got.ID)

	_, err = tm.GetByName("missing")
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerList(t *testing.T) {
	tm := NewTenantManager()
	_, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)
	_, err = tm.Create(context.Background(), "beta", "Beta", Quota{})
	require.NoError(t, err)

	list := tm.List()
	assert.Len(t, list, 2)

	// Verify the returned slice is a copy.
	list[0].Name = "mutated"
	again := tm.List()
	for _, tt := range again {
		assert.NotEqual(t, "mutated", tt.Name)
	}
}

func TestTenantManagerSuspend(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	require.NoError(t, tm.Suspend(context.Background(), tt.ID))
	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantSuspended, got.Status)

	// Suspending again is a no-op.
	require.NoError(t, tm.Suspend(context.Background(), tt.ID))
}

func TestTenantManagerSuspendMissing(t *testing.T) {
	tm := NewTenantManager()
	err := tm.Suspend(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerSuspendDeleted(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	require.NoError(t, tm.Delete(context.Background(), tt.ID))
	err = tm.Suspend(context.Background(), tt.ID)
	assert.ErrorIs(t, err, ErrTenantDeleted)
}

func TestTenantManagerResume(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	require.NoError(t, tm.Suspend(context.Background(), tt.ID))
	require.NoError(t, tm.Resume(context.Background(), tt.ID))
	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantActive, got.Status)

	// Resuming again is a no-op.
	require.NoError(t, tm.Resume(context.Background(), tt.ID))
}

func TestTenantManagerResumeMissing(t *testing.T) {
	tm := NewTenantManager()
	err := tm.Resume(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerResumeDeleted(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	require.NoError(t, tm.Delete(context.Background(), tt.ID))
	err = tm.Resume(context.Background(), tt.ID)
	assert.ErrorIs(t, err, ErrTenantDeleted)
}

func TestTenantManagerDelete(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: 5})
	require.NoError(t, err)

	require.NoError(t, tm.Delete(context.Background(), tt.ID))

	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantDeleted, got.Status)

	// Name should be released.
	_, err = tm.GetByName("acme")
	assert.ErrorIs(t, err, ErrTenantNotFound)

	// Quota should be removed.
	_, err = tm.QuotaManager().GetQuota(tt.ID)
	assert.ErrorIs(t, err, ErrQuotaNotFound)

	// Deleting again is a no-op.
	require.NoError(t, tm.Delete(context.Background(), tt.ID))
}

func TestTenantManagerDeleteMissing(t *testing.T) {
	tm := NewTenantManager()
	err := tm.Delete(context.Background(), "missing")
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerDeleteAndRecreateName(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)
	require.NoError(t, tm.Delete(context.Background(), tt.ID))

	// Reusing the name should produce a new tenant with a new id.
	tt2, err := tm.Create(context.Background(), "acme", "ACME 2", Quota{})
	require.NoError(t, err)
	assert.NotEqual(t, tt.ID, tt2.ID)
}

func TestTenantManagerUpdateQuota(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: 5})
	require.NoError(t, err)

	require.NoError(t, tm.UpdateQuota(context.Background(), tt.ID, Quota{MaxTargets: 50}))
	q, err := tm.QuotaManager().GetQuota(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, 50, q.MaxTargets)
}

func TestTenantManagerUpdateQuotaMissing(t *testing.T) {
	tm := NewTenantManager()
	err := tm.UpdateQuota(context.Background(), "missing", Quota{})
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerUpdateQuotaDeleted(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)
	require.NoError(t, tm.Delete(context.Background(), tt.ID))

	err = tm.UpdateQuota(context.Background(), tt.ID, Quota{MaxTargets: 10})
	assert.ErrorIs(t, err, ErrTenantDeleted)
}

func TestTenantManagerCheckQuota(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: 5})
	require.NoError(t, err)

	require.NoError(t, tm.CheckQuota(tt.ID, ResourceTargets, 5))
	err = tm.CheckQuota(tt.ID, ResourceTargets, 6)
	assert.ErrorIs(t, err, ErrQuotaExceeded)

	// Reserve some and check again.
	require.NoError(t, tm.QuotaManager().CheckAndReserve(tt.ID, ResourceTargets, 3))
	require.NoError(t, tm.CheckQuota(tt.ID, ResourceTargets, 2))
	err = tm.CheckQuota(tt.ID, ResourceTargets, 3)
	assert.ErrorIs(t, err, ErrQuotaExceeded)
}

func TestTenantManagerCheckQuotaUnlimited(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)
	require.NoError(t, tm.CheckQuota(tt.ID, ResourceTargets, 1_000_000))
}

func TestTenantManagerCheckQuotaMissing(t *testing.T) {
	tm := NewTenantManager()
	err := tm.CheckQuota("missing", ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrTenantNotFound)
}

func TestTenantManagerCheckQuotaSuspended(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: 5})
	require.NoError(t, err)
	require.NoError(t, tm.Suspend(context.Background(), tt.ID))

	err = tm.CheckQuota(tt.ID, ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrTenantSuspended)
}

func TestTenantManagerCheckQuotaDeleted(t *testing.T) {
	tm := NewTenantManager()
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{MaxTargets: 5})
	require.NoError(t, err)
	require.NoError(t, tm.Delete(context.Background(), tt.ID))

	err = tm.CheckQuota(tt.ID, ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrTenantDeleted)
}

func TestTenantManagerSetNow(t *testing.T) {
	tm := NewTenantManager()
	fixed := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	tm.SetNow(func() time.Time { return fixed })

	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{})
	require.NoError(t, err)

	require.NoError(t, tm.Suspend(context.Background(), tt.ID))
	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, fixed, got.UpdatedAt)

	// Restore default clock.
	tm.SetNow(nil)
	require.NoError(t, tm.Resume(context.Background(), tt.ID))
	got, err = tm.Get(tt.ID)
	require.NoError(t, err)
	assert.True(t, got.UpdatedAt.After(fixed))
}

func TestTenantManagerLifecycle(t *testing.T) {
	tm := NewTenantManager()

	// Create.
	tt, err := tm.Create(context.Background(), "acme", "ACME", Quota{
		MaxTargets:           10,
		MaxConcurrentChanges: 5,
		MaxStorageMB:         100,
		MaxAPIRatePerMin:     60,
	})
	require.NoError(t, err)
	assert.Equal(t, TenantActive, tt.Status)

	// Suspend.
	require.NoError(t, tm.Suspend(context.Background(), tt.ID))
	got, err := tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantSuspended, got.Status)

	// Resume.
	require.NoError(t, tm.Resume(context.Background(), tt.ID))
	got, err = tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantActive, got.Status)

	// Update quota.
	require.NoError(t, tm.UpdateQuota(context.Background(), tt.ID, Quota{MaxTargets: 20}))
	q, err := tm.QuotaManager().GetQuota(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, 20, q.MaxTargets)

	// Delete.
	require.NoError(t, tm.Delete(context.Background(), tt.ID))
	got, err = tm.Get(tt.ID)
	require.NoError(t, err)
	assert.Equal(t, TenantDeleted, got.Status)
}

// TestTenantManagerConcurrent exercises the manager under concurrent
// access to verify the mutex protects shared state.
func TestTenantManagerConcurrent(t *testing.T) {
	tm := NewTenantManager()

	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "tenant-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			_, _ = tm.Create(context.Background(), name, "Display", Quota{MaxTargets: 10})
		}(i)
	}
	wg.Wait()

	// All creates should have produced distinct tenants.
	assert.Equal(t, n, tm.Count())

	// Concurrent reads.
	var readWg sync.WaitGroup
	for i := 0; i < 20; i++ {
		readWg.Add(1)
		go func() {
			defer readWg.Done()
			_ = tm.List()
		}()
	}
	readWg.Wait()
}
