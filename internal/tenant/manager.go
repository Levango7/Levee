package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
)

// TenantManager owns the complete lifecycle of tenants: creation,
// suspension, resumption, soft-deletion, lookup and quota management.
// It is safe for concurrent use; all operations are guarded by a
// sync.RWMutex. The manager optionally accepts a state.Store for
// persistence; when nil, the manager runs in-memory (useful for tests
// and ephemeral CLI invocations).
type TenantManager struct {
	mu       sync.RWMutex
	tenants  map[string]*Tenant
	byName   map[string]string // name -> tenant id
	quotaMgr *QuotaManager
	now      func() time.Time // injectable clock for tests
}

// NewTenantManager returns an empty TenantManager. The returned manager
// is ready to use; tenants and quotas are added via Create.
func NewTenantManager() *TenantManager {
	return &TenantManager{
		tenants:  make(map[string]*Tenant),
		byName:   make(map[string]string),
		quotaMgr: NewQuotaManager(),
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// QuotaManager returns the underlying quota manager so that callers can
// perform quota checks (e.g. CheckAndReserve) against the same state
// used by the tenant manager.
func (tm *TenantManager) QuotaManager() *QuotaManager {
	return tm.quotaMgr
}

// Create creates a new tenant with the given name, display name and
// initial quota. The name must be unique and pass validation. On
// success the tenant is in TenantActive status and the quota is
// installed in the underlying QuotaManager.
func (tm *TenantManager) Create(ctx context.Context, name, displayName string, quota Quota) (*Tenant, error) {
	if tm == nil {
		return nil, errors.New("tenant: nil manager")
	}
	if name == "" {
		return nil, fmt.Errorf("%w: empty name", ErrInvalidTenant)
	}

	t := NewTenant(name, displayName)
	if err := t.Validate(); err != nil {
		return nil, err
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()

	if _, exists := tm.byName[name]; exists {
		return nil, fmt.Errorf("%w: name %q already in use", ErrTenantExists, name)
	}

	// Install the quota before publishing the tenant so that a quota
	// failure does not leave a half-registered tenant.
	if err := tm.quotaMgr.SetQuota(t.ID, quota); err != nil {
		return nil, fmt.Errorf("tenant: set initial quota: %w", err)
	}

	tm.tenants[t.ID] = t
	tm.byName[name] = t.ID

	log.InfoCtx(ctx, "tenant created",
		"tenant_id", t.ID, "name", t.Name)
	return t, nil
}

// Get returns the tenant with the given id. It returns ErrTenantNotFound
// when no such tenant exists (including soft-deleted tenants, which are
// retained in the map).
func (tm *TenantManager) Get(tenantID string) (*Tenant, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	t, ok := tm.tenants[tenantID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	cp := *t
	return &cp, nil
}

// GetByName returns the tenant with the given name. It returns
// ErrTenantNotFound when no such tenant exists.
func (tm *TenantManager) GetByName(name string) (*Tenant, error) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	id, ok := tm.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: name %s", ErrTenantNotFound, name)
	}
	t, ok := tm.tenants[id]
	if !ok {
		// Should never happen: byName and tenants are kept in sync.
		return nil, fmt.Errorf("%w: name %s maps to missing id %s", ErrTenantNotFound, name, id)
	}
	cp := *t
	return &cp, nil
}

// List returns all tenants (including soft-deleted ones) in an
// unspecified order. The returned slice is a copy; mutating it does not
// affect the manager.
func (tm *TenantManager) List() []*Tenant {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	result := make([]*Tenant, 0, len(tm.tenants))
	for _, t := range tm.tenants {
		cp := *t
		result = append(result, &cp)
	}
	return result
}

// Suspend transitions a tenant to TenantSuspended. It is a no-op when
// the tenant is already suspended. A deleted tenant cannot be
// suspended.
func (tm *TenantManager) Suspend(ctx context.Context, tenantID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, ok := tm.tenants[tenantID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	if t.Status == TenantDeleted {
		return fmt.Errorf("%w: cannot suspend deleted tenant %s", ErrTenantDeleted, tenantID)
	}
	if t.Status == TenantSuspended {
		return nil
	}

	t.Status = TenantSuspended
	t.UpdatedAt = tm.now()
	log.InfoCtx(ctx, "tenant suspended", "tenant_id", tenantID)
	return nil
}

// Resume transitions a tenant to TenantActive. It is a no-op when the
// tenant is already active. A deleted tenant cannot be resumed.
func (tm *TenantManager) Resume(ctx context.Context, tenantID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, ok := tm.tenants[tenantID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	if t.Status == TenantDeleted {
		return fmt.Errorf("%w: cannot resume deleted tenant %s", ErrTenantDeleted, tenantID)
	}
	if t.Status == TenantActive {
		return nil
	}

	t.Status = TenantActive
	t.UpdatedAt = tm.now()
	log.InfoCtx(ctx, "tenant resumed", "tenant_id", tenantID)
	return nil
}

// Delete soft-deletes a tenant: the status is set to TenantDeleted and
// the quota is removed from the QuotaManager so that the slots can be
// reused. The tenant record is retained so that historical runs and
// audits remain associated with a valid tenant identity. A deleted
// tenant cannot be recovered; re-creating a tenant with the same name
// is allowed and produces a new tenant id.
func (tm *TenantManager) Delete(ctx context.Context, tenantID string) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	t, ok := tm.tenants[tenantID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	if t.Status == TenantDeleted {
		return nil
	}

	t.Status = TenantDeleted
	t.UpdatedAt = tm.now()
	// Remove the name mapping so that the name can be reused.
	delete(tm.byName, t.Name)
	// Drop the quota so that the slots can be reused.
	tm.quotaMgr.RemoveQuota(tenantID)

	log.InfoCtx(ctx, "tenant deleted", "tenant_id", tenantID)
	return nil
}

// UpdateQuota replaces the quota for the given tenant. The tenant must
// exist and not be deleted.
func (tm *TenantManager) UpdateQuota(ctx context.Context, tenantID string, q Quota) error {
	tm.mu.RLock()
	t, ok := tm.tenants[tenantID]
	tm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	if t.Status == TenantDeleted {
		return fmt.Errorf("%w: cannot update quota for deleted tenant %s", ErrTenantDeleted, tenantID)
	}

	if err := tm.quotaMgr.SetQuota(tenantID, q); err != nil {
		return fmt.Errorf("tenant: update quota: %w", err)
	}
	log.InfoCtx(ctx, "tenant quota updated", "tenant_id", tenantID)
	return nil
}

// CheckQuota verifies that the tenant can reserve the given amount of
// the resource. It returns ErrTenantSuspended or ErrTenantDeleted when
// the tenant is not operational, and ErrQuotaExceeded when the limit
// would be exceeded. The check is non-mutating; use
// QuotaManager().CheckAndReserve to actually reserve capacity.
func (tm *TenantManager) CheckQuota(tenantID string, resource ResourceType, amount int) error {
	tm.mu.RLock()
	t, ok := tm.tenants[tenantID]
	tm.mu.RUnlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantNotFound, tenantID)
	}
	switch t.Status {
	case TenantSuspended:
		return fmt.Errorf("%w: %s", ErrTenantSuspended, tenantID)
	case TenantDeleted:
		return fmt.Errorf("%w: %s", ErrTenantDeleted, tenantID)
	}

	q, err := tm.quotaMgr.GetQuota(tenantID)
	if err != nil {
		return err
	}
	u, err := tm.quotaMgr.GetUsage(tenantID)
	if err != nil {
		return err
	}

	limit, err := q.LimitFor(resource)
	if err != nil {
		return err
	}
	if limit == 0 {
		// Unlimited.
		return nil
	}
	current, err := u.ValueFor(resource)
	if err != nil {
		return err
	}
	if current+amount > limit {
		return fmt.Errorf("%w: %s current=%d amount=%d limit=%d",
			ErrQuotaExceeded, resource, current, amount, limit)
	}
	return nil
}

// SetNow installs a custom clock function. It is intended for tests
// that need deterministic timestamps. Passing nil restores the default
// wall clock.
func (tm *TenantManager) SetNow(f func() time.Time) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if f == nil {
		tm.now = func() time.Time { return time.Now().UTC() }
		return
	}
	tm.now = f
}

// Count returns the number of tenants currently registered, including
// soft-deleted ones. It is primarily intended for diagnostics and
// tests.
func (tm *TenantManager) Count() int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return len(tm.tenants)
}
