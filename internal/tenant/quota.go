package tenant

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ResourceType enumerates the resource kinds that are subject to per-tenant
// quotas. The values are stable and used as map keys; do not renumber.
type ResourceType int

const (
	// ResourceTargets counts the number of target hosts a tenant has
	// registered.
	ResourceTargets ResourceType = iota
	// ResourceConcurrentChanges counts in-flight change runs.
	ResourceConcurrentChanges
	// ResourceStorage counts the total storage consumed by a tenant in
	// megabytes.
	ResourceStorage
	// ResourceAPIRate counts API requests in the current minute.
	ResourceAPIRate
)

// String returns the human-readable name of the resource type.
func (r ResourceType) String() string {
	switch r {
	case ResourceTargets:
		return "targets"
	case ResourceConcurrentChanges:
		return "concurrent_changes"
	case ResourceStorage:
		return "storage_mb"
	case ResourceAPIRate:
		return "api_rate_per_min"
	default:
		return fmt.Sprintf("unknown(%d)", int(r))
	}
}

// ParseResourceType converts a string to a ResourceType. Unknown values
// return an error.
func ParseResourceType(s string) (ResourceType, error) {
	switch s {
	case "targets":
		return ResourceTargets, nil
	case "concurrent_changes":
		return ResourceConcurrentChanges, nil
	case "storage_mb":
		return ResourceStorage, nil
	case "api_rate_per_min":
		return ResourceAPIRate, nil
	default:
		return 0, fmt.Errorf("tenant: unknown resource type %q", s)
	}
}

// Sentinel errors returned by quota operations.
var (
	// ErrQuotaExceeded is returned when an operation would push a
	// tenant's resource usage above its configured limit.
	ErrQuotaExceeded = errors.New("tenant: quota exceeded")
	// ErrQuotaNotFound is returned when no quota has been configured
	// for a tenant.
	ErrQuotaNotFound = errors.New("tenant: quota not found")
	// ErrInvalidQuota is returned when a quota value is negative or
	// otherwise invalid.
	ErrInvalidQuota = errors.New("tenant: invalid quota")
	// ErrInvalidResource is returned when an unknown ResourceType is
	// passed to a quota operation.
	ErrInvalidResource = errors.New("tenant: invalid resource")
)

// Quota defines the upper bounds for a tenant's resource consumption. A
// zero value for any field means "unlimited" for that resource; negative
// values are invalid and rejected by SetQuota.
type Quota struct {
	// TenantID is the tenant this quota applies to.
	TenantID string `json:"tenant_id"`
	// MaxTargets is the maximum number of target hosts. 0 means unlimited.
	MaxTargets int `json:"max_targets"`
	// MaxConcurrentChanges is the maximum number of in-flight change runs.
	// 0 means unlimited.
	MaxConcurrentChanges int `json:"max_concurrent_changes"`
	// MaxStorageMB is the maximum storage in megabytes. 0 means unlimited.
	MaxStorageMB int `json:"max_storage_mb"`
	// MaxAPIRatePerMin is the maximum API requests per minute. 0 means
	// unlimited.
	MaxAPIRatePerMin int `json:"max_api_rate_per_min"`
	// CreatedAt is the time the quota was first set.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the time the quota was last modified.
	UpdatedAt time.Time `json:"updated_at"`
}

// Validate checks that all quota fields are non-negative. A zero value is
// treated as "unlimited" and is valid.
func (q *Quota) Validate() error {
	if q == nil {
		return fmt.Errorf("%w: nil quota", ErrInvalidQuota)
	}
	if q.TenantID == "" {
		return fmt.Errorf("%w: empty tenant id", ErrInvalidQuota)
	}
	if q.MaxTargets < 0 {
		return fmt.Errorf("%w: max_targets is negative", ErrInvalidQuota)
	}
	if q.MaxConcurrentChanges < 0 {
		return fmt.Errorf("%w: max_concurrent_changes is negative", ErrInvalidQuota)
	}
	if q.MaxStorageMB < 0 {
		return fmt.Errorf("%w: max_storage_mb is negative", ErrInvalidQuota)
	}
	if q.MaxAPIRatePerMin < 0 {
		return fmt.Errorf("%w: max_api_rate_per_min is negative", ErrInvalidQuota)
	}
	return nil
}

// LimitFor returns the configured limit for the given resource. A return
// of 0 means unlimited.
func (q *Quota) LimitFor(r ResourceType) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("%w: nil quota", ErrInvalidQuota)
	}
	switch r {
	case ResourceTargets:
		return q.MaxTargets, nil
	case ResourceConcurrentChanges:
		return q.MaxConcurrentChanges, nil
	case ResourceStorage:
		return q.MaxStorageMB, nil
	case ResourceAPIRate:
		return q.MaxAPIRatePerMin, nil
	default:
		return 0, fmt.Errorf("%w: %d", ErrInvalidResource, int(r))
	}
}

// Usage represents the current consumption of a tenant's resources. It is
// maintained by QuotaManager and is always non-negative.
type Usage struct {
	// TenantID is the tenant this usage belongs to.
	TenantID string `json:"tenant_id"`
	// TargetCount is the current number of target hosts.
	TargetCount int `json:"target_count"`
	// ActiveChanges is the current number of in-flight change runs.
	ActiveChanges int `json:"active_changes"`
	// StorageUsedMB is the current storage consumption in megabytes.
	StorageUsedMB int `json:"storage_used_mb"`
	// APIRequestsThisMin is the number of API requests in the current
	// minute window.
	APIRequestsThisMin int `json:"api_requests_this_min"`
}

// ValueFor returns the current usage value for the given resource.
func (u *Usage) ValueFor(r ResourceType) (int, error) {
	if u == nil {
		return 0, fmt.Errorf("%w: nil usage", ErrInvalidQuota)
	}
	switch r {
	case ResourceTargets:
		return u.TargetCount, nil
	case ResourceConcurrentChanges:
		return u.ActiveChanges, nil
	case ResourceStorage:
		return u.StorageUsedMB, nil
	case ResourceAPIRate:
		return u.APIRequestsThisMin, nil
	default:
		return 0, fmt.Errorf("%w: %d", ErrInvalidResource, int(r))
	}
}

// QuotaManager tracks per-tenant quotas and current usage. It is safe for
// concurrent use; all operations are guarded by a sync.RWMutex. A
// QuotaManager is the single source of truth for quota enforcement: before
// performing a resource-consuming operation, callers should call
// CheckAndReserve, and after the operation completes (or fails) they
// should call Release to return the reserved capacity.
type QuotaManager struct {
	mu     sync.RWMutex
	quotas map[string]*Quota
	usage  map[string]*Usage
}

// NewQuotaManager returns an empty QuotaManager.
func NewQuotaManager() *QuotaManager {
	return &QuotaManager{
		quotas: make(map[string]*Quota),
		usage:  make(map[string]*Usage),
	}
}

// SetQuota installs or replaces the quota for a tenant. The quota must
// pass Validate; the TenantID field is overwritten with the given
// tenantID so callers cannot accidentally set a quota for the wrong
// tenant.
func (qm *QuotaManager) SetQuota(tenantID string, q Quota) error {
	if tenantID == "" {
		return fmt.Errorf("%w: empty tenant id", ErrInvalidQuota)
	}
	q.TenantID = tenantID
	if err := q.Validate(); err != nil {
		return err
	}

	now := time.Now().UTC()
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if existing, ok := qm.quotas[tenantID]; ok {
		q.CreatedAt = existing.CreatedAt
	} else {
		q.CreatedAt = now
	}
	q.UpdatedAt = now

	qCopy := q
	qm.quotas[tenantID] = &qCopy

	if _, ok := qm.usage[tenantID]; !ok {
		qm.usage[tenantID] = &Usage{TenantID: tenantID}
	}
	return nil
}

// GetQuota returns a copy of the quota for the given tenant. It returns
// ErrQuotaNotFound when no quota has been configured.
func (qm *QuotaManager) GetQuota(tenantID string) (*Quota, error) {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	q, ok := qm.quotas[tenantID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrQuotaNotFound, tenantID)
	}
	qCopy := *q
	return &qCopy, nil
}

// GetUsage returns a copy of the current usage for the given tenant. When
// no usage has been recorded, a zero usage is returned (the tenant is
// assumed to exist for the purpose of usage reporting).
func (qm *QuotaManager) GetUsage(tenantID string) (*Usage, error) {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	u, ok := qm.usage[tenantID]
	if !ok {
		return &Usage{TenantID: tenantID}, nil
	}
	uCopy := *u
	return &uCopy, nil
}

// CheckAndReserve verifies that the tenant's current usage of the given
// resource plus amount does not exceed the configured limit, and if so
// atomically increments the usage. A limit of 0 means unlimited. A
// negative amount is invalid. When the limit would be exceeded the
// usage is left unchanged and ErrQuotaExceeded is returned.
func (qm *QuotaManager) CheckAndReserve(tenantID string, resource ResourceType, amount int) error {
	if amount < 0 {
		return fmt.Errorf("%w: negative amount %d", ErrInvalidQuota, amount)
	}
	if amount == 0 {
		return nil
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	q, ok := qm.quotas[tenantID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrQuotaNotFound, tenantID)
	}
	u := qm.usage[tenantID]
	if u == nil {
		u = &Usage{TenantID: tenantID}
		qm.usage[tenantID] = u
	}

	limit, err := q.LimitFor(resource)
	if err != nil {
		return err
	}
	current, err := u.ValueFor(resource)
	if err != nil {
		return err
	}

	// limit == 0 means unlimited.
	if limit > 0 && current+amount > limit {
		return fmt.Errorf("%w: %s current=%d amount=%d limit=%d",
			ErrQuotaExceeded, resource, current, amount, limit)
	}

	return qm.applyDeltaLocked(u, resource, amount)
}

// Release decrements the usage of the given resource by amount. The
// usage is clamped at zero so a double-release does not produce negative
// usage. A negative amount is invalid.
func (qm *QuotaManager) Release(tenantID string, resource ResourceType, amount int) error {
	if amount < 0 {
		return fmt.Errorf("%w: negative amount %d", ErrInvalidQuota, amount)
	}
	if amount == 0 {
		return nil
	}

	qm.mu.Lock()
	defer qm.mu.Unlock()

	u, ok := qm.usage[tenantID]
	if !ok {
		// Nothing to release; treat as no-op.
		return nil
	}

	delta := -amount
	return qm.applyDeltaLocked(u, resource, delta)
}

// applyDeltaLocked mutates the usage by delta, clamping at zero. The
// caller must hold qm.mu.
func (qm *QuotaManager) applyDeltaLocked(u *Usage, resource ResourceType, delta int) error {
	switch resource {
	case ResourceTargets:
		u.TargetCount = clampNonNeg(u.TargetCount + delta)
	case ResourceConcurrentChanges:
		u.ActiveChanges = clampNonNeg(u.ActiveChanges + delta)
	case ResourceStorage:
		u.StorageUsedMB = clampNonNeg(u.StorageUsedMB + delta)
	case ResourceAPIRate:
		u.APIRequestsThisMin = clampNonNeg(u.APIRequestsThisMin + delta)
	default:
		return fmt.Errorf("%w: %d", ErrInvalidResource, int(resource))
	}
	return nil
}

// clampNonNeg returns v when v >= 0, else 0. It prevents negative usage
// arising from over-release.
func clampNonNeg(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// IsOverQuota reports whether the tenant's current usage of the given
// resource is at or above the configured limit. A limit of 0 (unlimited)
// is never over quota. When no quota is configured the tenant is
// considered over quota (safer default: refuse work rather than silently
// allow unbounded usage).
func (qm *QuotaManager) IsOverQuota(tenantID string, resource ResourceType) bool {
	qm.mu.RLock()
	defer qm.mu.RUnlock()

	q, ok := qm.quotas[tenantID]
	if !ok {
		return true
	}
	u := qm.usage[tenantID]
	if u == nil {
		// No usage recorded; cannot be over a positive limit.
		return false
	}

	limit, err := q.LimitFor(resource)
	if err != nil || limit == 0 {
		return false
	}
	current, err := u.ValueFor(resource)
	if err != nil {
		return false
	}
	return current >= limit
}

// ResetUsage zeroes the usage record for a tenant. It is primarily
// intended for tests and administrative operations (e.g. after
// reconciling with an external source of truth).
func (qm *QuotaManager) ResetUsage(tenantID string) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	qm.usage[tenantID] = &Usage{TenantID: tenantID}
}

// RemoveQuota deletes the quota and usage records for a tenant. It is
// called when a tenant is soft-deleted so that the quota slots can be
// reused. It is a no-op when the tenant has no quota.
func (qm *QuotaManager) RemoveQuota(tenantID string) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	delete(qm.quotas, tenantID)
	delete(qm.usage, tenantID)
}
