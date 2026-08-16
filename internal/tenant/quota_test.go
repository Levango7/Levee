package tenant

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceTypeString(t *testing.T) {
	cases := []struct {
		r    ResourceType
		want string
	}{
		{ResourceTargets, "targets"},
		{ResourceConcurrentChanges, "concurrent_changes"},
		{ResourceStorage, "storage_mb"},
		{ResourceAPIRate, "api_rate_per_min"},
		{ResourceType(99), "unknown(99)"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.r.String())
	}
}

func TestParseResourceType(t *testing.T) {
	cases := []struct {
		in   string
		want ResourceType
		err  bool
	}{
		{"targets", ResourceTargets, false},
		{"concurrent_changes", ResourceConcurrentChanges, false},
		{"storage_mb", ResourceStorage, false},
		{"api_rate_per_min", ResourceAPIRate, false},
		{"bogus", 0, true},
	}
	for _, c := range cases {
		got, err := ParseResourceType(c.in)
		if c.err {
			assert.Error(t, err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestQuotaValidate(t *testing.T) {
	cases := []struct {
		name    string
		quota   *Quota
		wantErr bool
	}{
		{"valid", &Quota{TenantID: "t1", MaxTargets: 10}, false},
		{"unlimited", &Quota{TenantID: "t1"}, false},
		{"nil", nil, true},
		{"empty tenant", &Quota{MaxTargets: 10}, true},
		{"negative targets", &Quota{TenantID: "t1", MaxTargets: -1}, true},
		{"negative changes", &Quota{TenantID: "t1", MaxConcurrentChanges: -1}, true},
		{"negative storage", &Quota{TenantID: "t1", MaxStorageMB: -1}, true},
		{"negative rate", &Quota{TenantID: "t1", MaxAPIRatePerMin: -1}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.quota.Validate()
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestQuotaLimitFor(t *testing.T) {
	q := &Quota{
		TenantID:             "t1",
		MaxTargets:           10,
		MaxConcurrentChanges: 5,
		MaxStorageMB:         100,
		MaxAPIRatePerMin:     60,
	}
	cases := []struct {
		r    ResourceType
		want int
	}{
		{ResourceTargets, 10},
		{ResourceConcurrentChanges, 5},
		{ResourceStorage, 100},
		{ResourceAPIRate, 60},
	}
	for _, c := range cases {
		got, err := q.LimitFor(c.r)
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}

	_, err := q.LimitFor(ResourceType(99))
	assert.Error(t, err)
}

func TestUsageValueFor(t *testing.T) {
	u := &Usage{
		TenantID:           "t1",
		TargetCount:        3,
		ActiveChanges:      2,
		StorageUsedMB:      50,
		APIRequestsThisMin: 30,
	}
	cases := []struct {
		r    ResourceType
		want int
	}{
		{ResourceTargets, 3},
		{ResourceConcurrentChanges, 2},
		{ResourceStorage, 50},
		{ResourceAPIRate, 30},
	}
	for _, c := range cases {
		got, err := u.ValueFor(c.r)
		require.NoError(t, err)
		assert.Equal(t, c.want, got)
	}
}

func TestQuotaManagerSetGet(t *testing.T) {
	qm := NewQuotaManager()
	q := Quota{MaxTargets: 10, MaxConcurrentChanges: 5}
	require.NoError(t, qm.SetQuota("t1", q))

	got, err := qm.GetQuota("t1")
	require.NoError(t, err)
	assert.Equal(t, "t1", got.TenantID)
	assert.Equal(t, 10, got.MaxTargets)
	assert.False(t, got.CreatedAt.IsZero())
	assert.False(t, got.UpdatedAt.IsZero())

	_, err = qm.GetQuota("missing")
	assert.ErrorIs(t, err, ErrQuotaNotFound)
}

func TestQuotaManagerSetQuotaInvalid(t *testing.T) {
	qm := NewQuotaManager()
	err := qm.SetQuota("t1", Quota{MaxTargets: -1})
	assert.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidQuota)

	err = qm.SetQuota("", Quota{})
	assert.Error(t, err)
}

func TestQuotaManagerSetQuotaPreservesCreatedAt(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 10}))
	first, err := qm.GetQuota("t1")
	require.NoError(t, err)

	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 20}))
	second, err := qm.GetQuota("t1")
	require.NoError(t, err)

	assert.Equal(t, first.CreatedAt, second.CreatedAt)
	assert.True(t, second.UpdatedAt.After(first.UpdatedAt) || second.UpdatedAt.Equal(first.UpdatedAt))
	assert.Equal(t, 20, second.MaxTargets)
}

func TestQuotaManagerCheckAndReserve(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))

	// Reserve up to the limit.
	for i := 0; i < 5; i++ {
		require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 1))
	}

	// Next reserve should fail.
	err := qm.CheckAndReserve("t1", ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrQuotaExceeded)

	// Usage should still be at the limit.
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 5, u.TargetCount)
}

func TestQuotaManagerCheckAndReserveUnlimited(t *testing.T) {
	qm := NewQuotaManager()
	// MaxTargets = 0 means unlimited.
	require.NoError(t, qm.SetQuota("t1", Quota{}))

	for i := 0; i < 100; i++ {
		require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 1))
	}
}

func TestQuotaManagerCheckAndReserveNoQuota(t *testing.T) {
	qm := NewQuotaManager()
	err := qm.CheckAndReserve("missing", ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrQuotaNotFound)
}

func TestQuotaManagerCheckAndReserveNegativeAmount(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))
	err := qm.CheckAndReserve("t1", ResourceTargets, -1)
	assert.Error(t, err)
}

func TestQuotaManagerCheckAndReserveZeroAmount(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))
	// Zero amount is a no-op.
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 0))
}

func TestQuotaManagerCheckAndReserveBatch(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 10}))

	// Reserve 7 in one call.
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 7))
	// Reserve 3 more.
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 3))
	// Reserve 1 more should fail.
	err := qm.CheckAndReserve("t1", ResourceTargets, 1)
	assert.ErrorIs(t, err, ErrQuotaExceeded)
}

func TestQuotaManagerRelease(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))

	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 5))
	require.NoError(t, qm.Release("t1", ResourceTargets, 2))

	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 3, u.TargetCount)

	// Now we can reserve again.
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 2))
}

func TestQuotaManagerReleaseClampsAtZero(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))

	// Release more than was reserved; usage should clamp at zero.
	require.NoError(t, qm.Release("t1", ResourceTargets, 10))
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 0, u.TargetCount)
}

func TestQuotaManagerReleaseNoQuota(t *testing.T) {
	qm := NewQuotaManager()
	// Release on a tenant with no recorded usage is a no-op.
	require.NoError(t, qm.Release("missing", ResourceTargets, 1))
}

func TestQuotaManagerReleaseNegativeAmount(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))
	err := qm.Release("t1", ResourceTargets, -1)
	assert.Error(t, err)
}

func TestQuotaManagerIsOverQuota(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 3}))

	assert.False(t, qm.IsOverQuota("t1", ResourceTargets))

	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 2))
	assert.False(t, qm.IsOverQuota("t1", ResourceTargets))

	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 1))
	assert.True(t, qm.IsOverQuota("t1", ResourceTargets))
}

func TestQuotaManagerIsOverQuotaUnlimited(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{})) // unlimited
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 1000))
	assert.False(t, qm.IsOverQuota("t1", ResourceTargets))
}

func TestQuotaManagerIsOverQuotaNoQuota(t *testing.T) {
	qm := NewQuotaManager()
	// No quota configured: safer to report over quota.
	assert.True(t, qm.IsOverQuota("missing", ResourceTargets))
}

func TestQuotaManagerResetUsage(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))
	require.NoError(t, qm.CheckAndReserve("t1", ResourceTargets, 3))

	qm.ResetUsage("t1")
	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 0, u.TargetCount)
}

func TestQuotaManagerRemoveQuota(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))

	qm.RemoveQuota("t1")
	_, err := qm.GetQuota("t1")
	assert.ErrorIs(t, err, ErrQuotaNotFound)
}

func TestQuotaManagerGetUsageMissing(t *testing.T) {
	qm := NewQuotaManager()
	u, err := qm.GetUsage("missing")
	require.NoError(t, err)
	assert.Equal(t, "missing", u.TenantID)
	assert.Equal(t, 0, u.TargetCount)
}

func TestQuotaManagerAllResourceTypes(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{
		MaxTargets:           2,
		MaxConcurrentChanges: 3,
		MaxStorageMB:         100,
		MaxAPIRatePerMin:     50,
	}))

	resources := []ResourceType{ResourceTargets, ResourceConcurrentChanges, ResourceStorage, ResourceAPIRate}
	for _, r := range resources {
		require.NoError(t, qm.CheckAndReserve("t1", r, 1), "reserve %s", r)
	}

	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 1, u.TargetCount)
	assert.Equal(t, 1, u.ActiveChanges)
	assert.Equal(t, 1, u.StorageUsedMB)
	assert.Equal(t, 1, u.APIRequestsThisMin)
}

func TestQuotaManagerInvalidResourceType(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 5}))
	err := qm.CheckAndReserve("t1", ResourceType(99), 1)
	assert.Error(t, err)
}

// TestQuotaManagerConcurrent exercises the QuotaManager under concurrent
// access to verify the mutex protects shared state.
func TestQuotaManagerConcurrent(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 1000}))

	var wg sync.WaitGroup
	accepted := make(chan struct{}, 2000)
	rejected := make(chan error, 2000)

	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				err := qm.CheckAndReserve("t1", ResourceTargets, 1)
				if err == nil {
					accepted <- struct{}{}
				} else {
					rejected <- err
				}
			}
		}()
	}

	wg.Wait()
	close(accepted)
	close(rejected)

	acceptCount := 0
	for range accepted {
		acceptCount++
	}
	rejectCount := 0
	for range rejected {
		rejectCount++
	}

	// Exactly 1000 should be accepted, the rest rejected.
	assert.Equal(t, 1000, acceptCount, "exactly the limit should be accepted")
	assert.Equal(t, 1000, rejectCount, "the rest should be rejected")

	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.Equal(t, 1000, u.TargetCount, "usage should match the limit")
}

// TestQuotaManagerConcurrentMixed exercises concurrent reserve and
// release operations to ensure the usage counter remains consistent.
func TestQuotaManagerConcurrentMixed(t *testing.T) {
	qm := NewQuotaManager()
	require.NoError(t, qm.SetQuota("t1", Quota{MaxTargets: 100}))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := qm.CheckAndReserve("t1", ResourceTargets, 1); err == nil {
					_ = qm.Release("t1", ResourceTargets, 1)
				}
			}
		}()
	}
	wg.Wait()

	u, err := qm.GetUsage("t1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, u.TargetCount, 0)
	assert.LessOrEqual(t, u.TargetCount, 100)
}

func ExampleQuotaManager() {
	qm := NewQuotaManager()
	_ = qm.SetQuota("t1", Quota{MaxTargets: 3})

	for i := 0; i < 3; i++ {
		_ = qm.CheckAndReserve("t1", ResourceTargets, 1)
	}
	err := qm.CheckAndReserve("t1", ResourceTargets, 1)
	fmt.Println("fourth reserve:", err)
	// Output: fourth reserve: tenant: quota exceeded: targets current=3 amount=1 limit=3
}
