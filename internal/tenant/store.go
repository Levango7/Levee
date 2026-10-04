package tenant

// store.go is the tenant-enforcing implementation of state.Store.
//
// WHAT CHANGED AND WHY: this replaces the previous IsolatedStore, which
// encoded tenant ownership into the free-form Run.IncidentID column using a
// "tenant:<id>|" prefix and then filtered results in Go AFTER the base store
// had already returned every row. That approach had three defects:
//
//  1. It could not filter anything that is not a Run. Credentials, target
//     hosts and inventory groups have no IncidentID, so they were never
//     isolated at all.
//  2. Filtering after the query means the database loads every tenant's rows
//     into the process before any of them are discarded.
//  3. It only implemented 12 of the Store interface's 79 methods, so it could
//     not be substituted for a Store — which is why it never shipped.
//
// The replacement delegates the filtering to SQL via the tenant_id columns, so
// a read is a plain indexed `AND tenant_id = ?` and the base store never
// materialises another tenant's rows. See Run.TenantID in internal/state for
// the schema rationale and the denormalisation decision.
//
// The tenant comes from the request context, resolved by Resolver, so one
// instance serves every tenant. See context.go for why the resolver fails
// closed rather than defaulting.

import (
	"context"
	"fmt"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// TenantStore enforces tenant isolation on every tenant-owned operation while
// leaving the platform-scoped tables untouched.
//
// It is a drop-in replacement for state.Store: a compile-time assertion below
// fails the build if a method is ever missing, so "can this actually be
// substituted" is a property of the compiler rather than of a review.
type TenantStore struct {
	base state.Store
	res  *Resolver
	qm   *QuotaManager
}

var _ state.Store = (*TenantStore)(nil)

// NewTenantStore wraps base with tenant enforcement. A nil resolver is treated
// as disabled (single-tenant), which keeps a partially constructed server from
// accidentally enforcing isolation on a deployment that never opted in. The
// quota manager is optional.
func NewTenantStore(base state.Store, res *Resolver, qm *QuotaManager) *TenantStore {
	return &TenantStore{base: base, res: res, qm: qm}
}

// Base returns the underlying store. Callers must not use it to serve a
// tenant-scoped request: it has no isolation whatsoever.
func (s *TenantStore) Base() state.Store { return s.base }

// Underlying implements state.Unwrapper so that code which has to know what
// database is in use — the change calendar's dialect choice, `system status`'s
// backend label — can see through this decorator instead of reporting "unknown
// store type". Without it, turning on `tenant.enabled` makes the freeze-period
// gate inert, because internal/state cannot import this package to ask.
//
// It is deliberately not a way to bypass isolation: it returns the same handle
// Base does, and only type identification should use it.
func (s *TenantStore) Underlying() state.Store { return s.base }

var _ state.Unwrapper = (*TenantStore)(nil)

// Resolver returns the resolver this store enforces.
func (s *TenantStore) Resolver() *Resolver { return s.res }

func (s *TenantStore) notReady() error {
	if s == nil || s.base == nil {
		return fmt.Errorf("tenant: store not initialised")
	}
	return nil
}

// scope resolves the tenant for this call and returns a context that carries
// it. Every tenant-scoped method starts here, so there is exactly one place
// where "which tenant is this call" is decided.
func (s *TenantStore) scope(ctx context.Context) (context.Context, string, error) {
	if err := s.notReady(); err != nil {
		return nil, "", err
	}
	return s.res.Scope(ctx)
}

// ownRunTenant loads a run and reports whether it belongs to tenantID.
//
// Every run-scoped child write goes through this. Without it, a caller could
// create a step inside another tenant's run: the step would be stamped with
// the caller's tenant while its parent run belongs to someone else, breaking
// the child/parent tenant agreement that TenantConsistency asserts, and
// hiding a real execution record from the tenant that owns the run.
func (s *TenantStore) ownRunTenant(ctx context.Context, tenantID, runID string) error {
	if runID == "" {
		return nil
	}
	run, err := s.base.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("tenant: load run %q for ownership check: %w", runID, err)
	}
	if run == nil {
		return fmt.Errorf("%w: run %q not found", ErrIsolationViolation, runID)
	}
	if run.TenantID != tenantID {
		log.WarnCtx(ctx, "cross-tenant child write denied",
			"tenant_id", tenantID, "run_id", runID, "owner", run.TenantID)
		return fmt.Errorf("%w: run %q belongs to tenant %q", ErrCrossTenantAccess, runID, run.TenantID)
	}
	return nil
}

// ownRunByID loads a run and requires it to belong to tenantID. It returns
// (nil, nil) when the run does not exist, and ErrCrossTenantAccess when it
// exists but belongs to someone else.
func (s *TenantStore) ownRunByID(ctx context.Context, tenantID, id string) (*state.Run, error) {
	run, err := s.base.GetRun(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("tenant: load run %q for ownership check: %w", id, err)
	}
	if run == nil {
		return nil, nil
	}
	if run.TenantID != tenantID {
		log.WarnCtx(ctx, "cross-tenant access denied",
			"tenant_id", tenantID, "run_id", id, "owner", run.TenantID)
		return nil, fmt.Errorf("%w: run %q belongs to tenant %q", ErrCrossTenantAccess, id, run.TenantID)
	}
	return run, nil
}

// ownRunByIDRequires is ownRunByID for call sites that only need the check,
// not the row. A missing run is an error here (the caller is about to mutate
// it) whereas ownRunByID returns (nil, nil).
func (s *TenantStore) ownRunByIDRequires(ctx context.Context, tenantID, id string) error {
	run, err := s.ownRunByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("%w: run %q not found", ErrIsolationViolation, id)
	}
	return nil
}

// --- Run CRUD ---------------------------------------------------------------

// CreateRun stamps the resolved tenant onto the run. The quota reservation is
// taken BEFORE the insert so a rejected request leaves no half-created record.
func (s *TenantStore) CreateRun(ctx context.Context, run *state.Run) error {
	if err := s.notReady(); err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("tenant: nil run")
	}
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if s.qm != nil {
		if err := s.qm.CheckAndReserve(tid, ResourceConcurrentChanges, 1); err != nil {
			return fmt.Errorf("tenant: reserve concurrent change: %w", err)
		}
	}
	run.TenantID = tid
	if err := s.base.CreateRun(ctx, run); err != nil {
		if s.qm != nil {
			_ = s.qm.Release(tid, ResourceConcurrentChanges, 1)
		}
		return err
	}
	return nil
}

// GetRun returns (nil, nil) for a run owned by another tenant, so a caller
// cannot distinguish "does not exist" from "belongs to someone else" — the
// same non-existence signal the previous implementation used.
func (s *TenantStore) GetRun(ctx context.Context, id string) (*state.Run, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	run, err := s.base.GetRun(ctx, id)
	if err != nil || run == nil {
		return nil, err
	}
	if run.TenantID != tid {
		log.WarnCtx(ctx, "cross-tenant get run denied", "tenant_id", tid, "run_id", id)
		return nil, nil
	}
	return run, nil
}

func (s *TenantStore) UpdateRun(ctx context.Context, run *state.Run) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if run == nil {
		return fmt.Errorf("tenant: nil run")
	}
	if err := s.ownRunByIDRequires(ctx, tid, run.ID); err != nil {
		return err
	}
	// Force the tenant back to the resolved one: a caller must not be able to
	// move a run into another tenant by setting the field on the struct.
	run.TenantID = tid
	return s.base.UpdateRun(ctx, run)
}

// UpdateRunStatusIf, UpdateRunApprovalStatusIf and UpdateRunPlan are narrow
// status/plan writes on a run the caller must already hold. Each verifies
// ownership and then passes through; none of them touches tenant_id, so none
// of them can move a row between tenants.
func (s *TenantStore) UpdateRunStatusIf(ctx context.Context, id, from, to string, updatedAt time.Time) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if _, err := s.ownRunByID(ctx, tid, id); err != nil {
		return false, err
	}
	return s.base.UpdateRunStatusIf(ctx, id, from, to, updatedAt)
}

func (s *TenantStore) UpdateRunApprovalStatusIf(ctx context.Context, id, from, to, approvalStatus string, updatedAt time.Time) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if _, err := s.ownRunByID(ctx, tid, id); err != nil {
		return false, err
	}
	return s.base.UpdateRunApprovalStatusIf(ctx, id, from, to, approvalStatus, updatedAt)
}

func (s *TenantStore) UpdateRunPlan(ctx context.Context, id, planJSON, planHash string, updatedAt time.Time) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if _, err := s.ownRunByID(ctx, tid, id); err != nil {
		return err
	}
	return s.base.UpdateRunPlan(ctx, id, planJSON, planHash, updatedAt)
}

func (s *TenantStore) MarkNonTerminalSteps(ctx context.Context, runID, marker string) (int64, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.ownRunTenant(ctx, tid, runID); err != nil {
		return 0, err
	}
	return s.base.MarkNonTerminalSteps(ctx, runID, marker)
}

func (s *TenantStore) ListRuns(ctx context.Context, filter state.RunFilter) ([]*state.Run, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	// Override rather than AND: a caller-supplied TenantID must not be able to
	// widen or narrow the scope the resolver decided.
	filter.TenantID = tid
	return s.base.ListRuns(ctx, filter)
}

func (s *TenantStore) DeleteRun(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if err := s.ownRunByIDRequires(ctx, tid, id); err != nil {
		return err
	}
	if err := s.base.DeleteRun(ctx, id); err != nil {
		return err
	}
	if s.qm != nil {
		_ = s.qm.Release(tid, ResourceConcurrentChanges, 1)
	}
	return nil
}

// --- Batch ------------------------------------------------------------------

func (s *TenantStore) CreateBatch(ctx context.Context, b *state.Batch) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("tenant: nil batch")
	}
	if err := s.ownRunTenant(ctx, tid, b.RunID); err != nil {
		return err
	}
	b.TenantID = tid
	return s.base.CreateBatch(ctx, b)
}

func (s *TenantStore) GetBatch(ctx context.Context, id string) (*state.Batch, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	b, err := s.base.GetBatch(ctx, id)
	if err != nil || b == nil {
		return nil, err
	}
	if b.TenantID != tid {
		return nil, nil
	}
	return b, nil
}

func (s *TenantStore) UpdateBatch(ctx context.Context, b *state.Batch) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if b == nil {
		return fmt.Errorf("tenant: nil batch")
	}
	if b.TenantID != "" && b.TenantID != tid {
		return fmt.Errorf("%w: batch %q belongs to tenant %q", ErrCrossTenantAccess, b.ID, b.TenantID)
	}
	if err := s.ownRunTenant(ctx, tid, b.RunID); err != nil {
		return err
	}
	b.TenantID = tid
	return s.base.UpdateBatch(ctx, b)
}

func (s *TenantStore) ListBatches(ctx context.Context, filter state.BatchFilter) ([]*state.Batch, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListBatches(ctx, filter)
}

func (s *TenantStore) DeleteBatch(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	b, err := s.base.GetBatch(ctx, id)
	if err != nil {
		return err
	}
	if b != nil && b.TenantID != tid {
		return fmt.Errorf("%w: batch %q belongs to tenant %q", ErrCrossTenantAccess, id, b.TenantID)
	}
	return s.base.DeleteBatch(ctx, id)
}

// --- Step -------------------------------------------------------------------

func (s *TenantStore) CreateStep(ctx context.Context, st *state.Step) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("tenant: nil step")
	}
	if err := s.ownRunTenant(ctx, tid, st.RunID); err != nil {
		return err
	}
	st.TenantID = tid
	return s.base.CreateStep(ctx, st)
}

func (s *TenantStore) GetStep(ctx context.Context, id string) (*state.Step, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	st, err := s.base.GetStep(ctx, id)
	if err != nil || st == nil {
		return nil, err
	}
	if st.TenantID != tid {
		return nil, nil
	}
	return st, nil
}

func (s *TenantStore) UpdateStep(ctx context.Context, st *state.Step) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("tenant: nil step")
	}
	if err := s.ownRunTenant(ctx, tid, st.RunID); err != nil {
		return err
	}
	st.TenantID = tid
	return s.base.UpdateStep(ctx, st)
}

func (s *TenantStore) ListSteps(ctx context.Context, filter state.StepFilter) ([]*state.Step, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListSteps(ctx, filter)
}

func (s *TenantStore) DeleteStep(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	st, err := s.base.GetStep(ctx, id)
	if err != nil {
		return err
	}
	if st != nil && st.TenantID != tid {
		return fmt.Errorf("%w: step %q belongs to tenant %q", ErrCrossTenantAccess, id, st.TenantID)
	}
	return s.base.DeleteStep(ctx, id)
}

// --- Trace ------------------------------------------------------------------

func (s *TenantStore) CreateTrace(ctx context.Context, tr *state.Trace) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if tr == nil {
		return fmt.Errorf("tenant: nil trace")
	}
	if err := s.ownRunTenant(ctx, tid, tr.RunID); err != nil {
		return err
	}
	tr.TenantID = tid
	return s.base.CreateTrace(ctx, tr)
}

func (s *TenantStore) GetTrace(ctx context.Context, id string) (*state.Trace, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := s.base.GetTrace(ctx, id)
	if err != nil || tr == nil {
		return nil, err
	}
	if tr.TenantID != tid {
		return nil, nil
	}
	return tr, nil
}

// UpdateTrace rebinds the RESOLVED tenant, which for an owned trace equals the
// stored one. Passing a Trace whose TenantID differs from the stored row would
// trip the WORM trigger — and that is the intended outcome: tenant_id is
// immutable from insert, and an attempt to move a trace between tenants must
// fail loudly rather than silently succeed.
func (s *TenantStore) UpdateTrace(ctx context.Context, tr *state.Trace) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if tr == nil {
		return fmt.Errorf("tenant: nil trace")
	}
	if err := s.ownRunTenant(ctx, tid, tr.RunID); err != nil {
		return err
	}
	tr.TenantID = tid
	return s.base.UpdateTrace(ctx, tr)
}

func (s *TenantStore) UpdateTraceChecksum(ctx context.Context, id, checksum string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	tr, err := s.base.GetTrace(ctx, id)
	if err != nil {
		return err
	}
	if tr != nil && tr.TenantID != tid {
		return fmt.Errorf("%w: trace %q belongs to tenant %q", ErrCrossTenantAccess, id, tr.TenantID)
	}
	return s.base.UpdateTraceChecksum(ctx, id, checksum)
}

func (s *TenantStore) ListTraces(ctx context.Context, filter state.TraceFilter) ([]*state.Trace, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListTraces(ctx, filter)
}

// DeleteTrace is a pass-through: the database WORM trigger rejects the delete
// regardless. The ownership check exists so the caller gets a tenant error
// rather than a generic WORM one when the row is not even theirs.
func (s *TenantStore) DeleteTrace(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	tr, err := s.base.GetTrace(ctx, id)
	if err != nil {
		return err
	}
	if tr != nil && tr.TenantID != tid {
		return fmt.Errorf("%w: trace %q belongs to tenant %q", ErrCrossTenantAccess, id, tr.TenantID)
	}
	return s.base.DeleteTrace(ctx, id)
}

// --- Approval ---------------------------------------------------------------

func (s *TenantStore) CreateApproval(ctx context.Context, a *state.Approval) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("tenant: nil approval")
	}
	if err := s.ownRunTenant(ctx, tid, a.RunID); err != nil {
		return err
	}
	a.TenantID = tid
	return s.base.CreateApproval(ctx, a)
}

func (s *TenantStore) GetApproval(ctx context.Context, id string) (*state.Approval, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.base.GetApproval(ctx, id)
	if err != nil || a == nil {
		return nil, err
	}
	if a.TenantID != tid {
		return nil, nil
	}
	return a, nil
}

func (s *TenantStore) UpdateApproval(ctx context.Context, a *state.Approval) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("tenant: nil approval")
	}
	if err := s.ownRunTenant(ctx, tid, a.RunID); err != nil {
		return err
	}
	a.TenantID = tid
	return s.base.UpdateApproval(ctx, a)
}

func (s *TenantStore) UpdateApprovalIfPending(ctx context.Context, a *state.Approval) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if a == nil {
		return false, fmt.Errorf("tenant: nil approval")
	}
	if err := s.ownRunTenant(ctx, tid, a.RunID); err != nil {
		return false, err
	}
	a.TenantID = tid
	return s.base.UpdateApprovalIfPending(ctx, a)
}

func (s *TenantStore) ListApprovals(ctx context.Context, filter state.ApprovalFilter) ([]*state.Approval, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListApprovals(ctx, filter)
}

func (s *TenantStore) DeleteApproval(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	a, err := s.base.GetApproval(ctx, id)
	if err != nil {
		return err
	}
	if a != nil && a.TenantID != tid {
		return fmt.Errorf("%w: approval %q belongs to tenant %q", ErrCrossTenantAccess, id, a.TenantID)
	}
	return s.base.DeleteApproval(ctx, id)
}

// --- Lock (platform-scoped, deliberately NOT tenant-filtered) ----------------

// The lock table has no tenant_id and never will. A lock guards one physical
// host; if it were scoped per tenant, tenant A holding host:db-1 and tenant B
// holding host:db-1 would be two rows and both would be admitted, which is
// precisely the concurrent-change collision the lock exists to prevent.
// These methods are therefore pass-throughs.

func (s *TenantStore) CreateLock(ctx context.Context, l *state.Lock) error {
	if err := s.notReady(); err != nil {
		return err
	}
	if _, _, err := s.scope(ctx); err != nil {
		return err
	}
	return s.base.CreateLock(ctx, l)
}
func (s *TenantStore) GetLock(ctx context.Context, id string) (*state.Lock, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return nil, err
	}
	return s.base.GetLock(ctx, id)
}
func (s *TenantStore) GetLockByScope(ctx context.Context, scope string) (*state.Lock, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return nil, err
	}
	return s.base.GetLockByScope(ctx, scope)
}
func (s *TenantStore) UpdateLock(ctx context.Context, l *state.Lock) error {
	if _, _, err := s.scope(ctx); err != nil {
		return err
	}
	return s.base.UpdateLock(ctx, l)
}
func (s *TenantStore) UpdateLockOwnedBy(ctx context.Context, id, owner string, ttlSeconds int, now time.Time) (int64, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return 0, err
	}
	return s.base.UpdateLockOwnedBy(ctx, id, owner, ttlSeconds, now)
}
func (s *TenantStore) ListLocks(ctx context.Context) ([]*state.Lock, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return nil, err
	}
	return s.base.ListLocks(ctx)
}
func (s *TenantStore) DeleteLock(ctx context.Context, id string) error {
	if _, _, err := s.scope(ctx); err != nil {
		return err
	}
	return s.base.DeleteLock(ctx, id)
}
func (s *TenantStore) DeleteLockByIDAndOwner(ctx context.Context, id, owner string) (bool, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return false, err
	}
	return s.base.DeleteLockByIDAndOwner(ctx, id, owner)
}
func (s *TenantStore) DeleteExpiredLocks(ctx context.Context, now time.Time) (int64, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return 0, err
	}
	return s.base.DeleteExpiredLocks(ctx, now)
}

// --- Credential -------------------------------------------------------------

// Credentials are tenant-owned secrets, so every path here is scoped. Note
// that GetCredentialByName cannot be: the name column is UNIQUE table-wide,
// so there is no filter to add and no way to express "this name, for me"
// without a signature change. It is ownership-checked on the way out instead,
// which is equivalent for a single-row lookup and leaks nothing an error
// message would not already leak.
func (s *TenantStore) CreateCredential(ctx context.Context, c *state.Credential) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("tenant: nil credential")
	}
	c.TenantID = tid
	return s.base.CreateCredential(ctx, c)
}

func (s *TenantStore) GetCredential(ctx context.Context, id string) (*state.Credential, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.base.GetCredential(ctx, id)
	if err != nil || c == nil {
		return nil, err
	}
	if c.TenantID != tid {
		log.WarnCtx(ctx, "cross-tenant credential read denied", "tenant_id", tid, "credential_id", id)
		return nil, nil
	}
	return c, nil
}

func (s *TenantStore) GetCredentialByName(ctx context.Context, name string) (*state.Credential, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.base.GetCredentialByName(ctx, name)
	if err != nil || c == nil {
		return nil, err
	}
	if c.TenantID != tid {
		return nil, nil
	}
	return c, nil
}

func (s *TenantStore) UpdateCredential(ctx context.Context, c *state.Credential) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("tenant: nil credential")
	}
	existing, err := s.base.GetCredential(ctx, c.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.TenantID != tid {
		return fmt.Errorf("%w: credential %q belongs to tenant %q", ErrCrossTenantAccess, c.ID, existing.TenantID)
	}
	c.TenantID = tid
	return s.base.UpdateCredential(ctx, c)
}

// ListCredentials filters in memory. The base method takes no filter struct,
// so there is nowhere to put a SQL predicate without changing the Store
// interface and every implementation and caller of it. The rows are AES-GCM
// ciphertext, so nothing sensitive leaves the process during the scan; the
// filter is applied before any caller sees a pointer.
func (s *TenantStore) ListCredentials(ctx context.Context) ([]*state.Credential, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	all, err := s.base.ListCredentials(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*state.Credential, 0, len(all))
	for _, c := range all {
		if c.TenantID == tid {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *TenantStore) DeleteCredential(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	c, err := s.base.GetCredential(ctx, id)
	if err != nil {
		return err
	}
	if c != nil && c.TenantID != tid {
		return fmt.Errorf("%w: credential %q belongs to tenant %q", ErrCrossTenantAccess, id, c.TenantID)
	}
	return s.base.DeleteCredential(ctx, id)
}

// --- Audit ------------------------------------------------------------------

func (s *TenantStore) CreateAudit(ctx context.Context, a *state.Audit) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("tenant: nil audit")
	}
	// A run-less audit entry (login, config, credential) has no parent to
	// check and simply belongs to the calling tenant.
	if a.RunID != "" {
		if err := s.ownRunTenant(ctx, tid, a.RunID); err != nil {
			return err
		}
	}
	a.TenantID = tid
	return s.base.CreateAudit(ctx, a)
}

func (s *TenantStore) GetAudit(ctx context.Context, id string) (*state.Audit, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.base.GetAudit(ctx, id)
	if err != nil || a == nil {
		return nil, err
	}
	if a.TenantID != tid {
		return nil, nil
	}
	return a, nil
}

// UpdateAuditChain is a pass-through with an ownership check: the chain
// hashes describe WHERE a row sits in the log, not anything tenant-specific,
// and the hashes themselves must be written by the chain builder rather than
// recomputed per tenant (a per-tenant recomputation would produce different
// hashes for the same row and break the global chain).
func (s *TenantStore) UpdateAuditChain(ctx context.Context, id string, prevHash string, currHash string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	a, err := s.base.GetAudit(ctx, id)
	if err != nil {
		return err
	}
	if a != nil && a.TenantID != tid {
		return fmt.Errorf("%w: audit %q belongs to tenant %q", ErrCrossTenantAccess, id, a.TenantID)
	}
	return s.base.UpdateAuditChain(ctx, id, prevHash, currHash)
}

func (s *TenantStore) ListAudits(ctx context.Context, filter state.AuditFilter) ([]*state.Audit, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListAudits(ctx, filter)
}

// --- Assignment (cross-node dispatch) ---------------------------------------

func (s *TenantStore) CreateAssignment(ctx context.Context, a *state.Assignment) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("tenant: nil assignment")
	}
	if err := s.ownRunTenant(ctx, tid, a.RunID); err != nil {
		return err
	}
	a.TenantID = tid
	return s.base.CreateAssignment(ctx, a)
}

func (s *TenantStore) GetAssignment(ctx context.Context, runID string) (*state.Assignment, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.base.GetAssignment(ctx, runID)
	if err != nil || a == nil {
		return nil, err
	}
	if a.TenantID != tid {
		return nil, nil
	}
	return a, nil
}

func (s *TenantStore) ListAssignments(ctx context.Context, filter state.AssignmentFilter) ([]*state.Assignment, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListAssignments(ctx, filter)
}

func (s *TenantStore) DeleteAssignment(ctx context.Context, runID string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	a, err := s.base.GetAssignment(ctx, runID)
	if err != nil {
		return err
	}
	if a != nil && a.TenantID != tid {
		return fmt.Errorf("%w: assignment for run %q belongs to tenant %q", ErrCrossTenantAccess, runID, a.TenantID)
	}
	return s.base.DeleteAssignment(ctx, runID)
}

// The assignment state transitions are narrow CAS statements on a run_id the
// dispatch loop already owns. Each verifies the assignment's tenant and then
// passes through untouched; none of them can move a row between tenants
// because none of them writes tenant_id.
func (s *TenantStore) assignmentOwned(ctx context.Context, runID, tid string) error {
	a, err := s.base.GetAssignment(ctx, runID)
	if err != nil {
		return err
	}
	if a == nil {
		return nil // not yet assigned; the CAS below will simply not match
	}
	if a.TenantID != tid {
		return fmt.Errorf("%w: assignment for run %q belongs to tenant %q", ErrCrossTenantAccess, runID, a.TenantID)
	}
	return nil
}

func (s *TenantStore) UpdateAssignmentStateIf(ctx context.Context, runID string, epoch int64, expected, next string) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if err := s.assignmentOwned(ctx, runID, tid); err != nil {
		return false, err
	}
	return s.base.UpdateAssignmentStateIf(ctx, runID, epoch, expected, next)
}

func (s *TenantStore) UpdateAssignmentState(ctx context.Context, runID, next string) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if err := s.assignmentOwned(ctx, runID, tid); err != nil {
		return false, err
	}
	return s.base.UpdateAssignmentState(ctx, runID, next)
}

func (s *TenantStore) Reassign(ctx context.Context, runID string, prevEpoch int64, newNode string) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if err := s.assignmentOwned(ctx, runID, tid); err != nil {
		return false, err
	}
	return s.base.Reassign(ctx, runID, prevEpoch, newNode)
}

func (s *TenantStore) ReclaimAssignment(ctx context.Context, runID string, epoch int64, newNode string) (bool, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return false, err
	}
	if err := s.assignmentOwned(ctx, runID, tid); err != nil {
		return false, err
	}
	return s.base.ReclaimAssignment(ctx, runID, epoch, newNode)
}

func (s *TenantStore) SetAssignmentResult(ctx context.Context, runID string, epoch int64, result string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if err := s.assignmentOwned(ctx, runID, tid); err != nil {
		return err
	}
	return s.base.SetAssignmentResult(ctx, runID, epoch, result)
}

// --- Cluster (platform-scoped) ----------------------------------------------

// Cluster membership and the assignment roll-up are operator-level views of
// the whole deployment, not tenant data. They stay global; a tenant-facing
// cluster view, if one is ever needed, is a separate filtered projection and
// not something to smuggle in here.
func (s *TenantStore) ListClusterNodes(ctx context.Context) ([]state.ClusterNode, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return nil, err
	}
	return s.base.ListClusterNodes(ctx)
}

func (s *TenantStore) AssignmentSummary(ctx context.Context) (*state.AssignmentSummary, error) {
	if _, _, err := s.scope(ctx); err != nil {
		return nil, err
	}
	return s.base.AssignmentSummary(ctx)
}

// BatchSummary is per-run, so it IS tenant-scoped: it exposes batch progress
// for a run, which is a change record.
func (s *TenantStore) BatchSummary(ctx context.Context, runID string) (*state.BatchSummary, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.ownRunTenant(ctx, tid, runID); err != nil {
		return nil, err
	}
	return s.base.BatchSummary(ctx, runID)
}

// --- Inventory groups -------------------------------------------------------

func (s *TenantStore) UpsertInventoryGroup(ctx context.Context, g *state.InventoryGroup) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("tenant: nil inventory group")
	}
	if g.TenantID != "" && g.TenantID != tid {
		return fmt.Errorf("%w: group %q belongs to tenant %q", ErrCrossTenantAccess, g.ID, g.TenantID)
	}
	existing, err := s.base.GetInventoryGroup(ctx, g.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.TenantID != "" && existing.TenantID != tid {
		return fmt.Errorf("%w: group %q belongs to tenant %q", ErrCrossTenantAccess, g.ID, existing.TenantID)
	}
	g.TenantID = tid
	return s.base.UpsertInventoryGroup(ctx, g)
}

func (s *TenantStore) GetInventoryGroup(ctx context.Context, id string) (*state.InventoryGroup, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.base.GetInventoryGroup(ctx, id)
	if err != nil || g == nil {
		return nil, err
	}
	if g.TenantID != tid {
		return nil, nil
	}
	return g, nil
}

// GetInventoryGroupByName: name is UNIQUE table-wide, so the lookup is
// already single-row; ownership is checked on the way out. See the equivalent
// note on GetCredentialByName.
func (s *TenantStore) GetInventoryGroupByName(ctx context.Context, name string) (*state.InventoryGroup, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	g, err := s.base.GetInventoryGroupByName(ctx, name)
	if err != nil || g == nil {
		return nil, err
	}
	if g.TenantID != tid {
		return nil, nil
	}
	return g, nil
}

// ListInventoryGroups filters in memory for the same reason as
// ListCredentials: the base method takes no filter struct.
func (s *TenantStore) ListInventoryGroups(ctx context.Context) ([]*state.InventoryGroup, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	all, err := s.base.ListInventoryGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*state.InventoryGroup, 0, len(all))
	for _, g := range all {
		if g.TenantID == tid {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *TenantStore) DeleteInventoryGroup(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	g, err := s.base.GetInventoryGroup(ctx, id)
	if err != nil {
		return err
	}
	if g != nil && g.TenantID != tid {
		return fmt.Errorf("%w: group %q belongs to tenant %q", ErrCrossTenantAccess, id, g.TenantID)
	}
	return s.base.DeleteInventoryGroup(ctx, id)
}

// --- Targets ----------------------------------------------------------------

// A target is tenant-owned, but the lock that guards it is not: see the Lock
// section. A host is one physical machine, so (hostname, port) stays UNIQUE
// table-wide and two tenants may not both register the same address even
// though neither can see the other's row.

func (s *TenantStore) UpsertTarget(ctx context.Context, t *state.Target) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("tenant: nil target")
	}
	if t.TenantID != "" && t.TenantID != tid {
		return fmt.Errorf("%w: target %q belongs to tenant %q", ErrCrossTenantAccess, t.ID, t.TenantID)
	}
	existing, err := s.base.GetTarget(ctx, t.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.TenantID != "" && existing.TenantID != tid {
		return fmt.Errorf("%w: target %q belongs to tenant %q", ErrCrossTenantAccess, t.ID, existing.TenantID)
	}
	t.TenantID = tid
	return s.base.UpsertTarget(ctx, t)
}

func (s *TenantStore) GetTarget(ctx context.Context, id string) (*state.Target, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.base.GetTarget(ctx, id)
	if err != nil || t == nil {
		return nil, err
	}
	if t.TenantID != tid {
		return nil, nil
	}
	return t, nil
}

// FindTargetByAddress is the one cross-tenant-sensitive lookup that cannot be
// scoped: (hostname, port) is UNIQUE table-wide so the base call already
// resolves to at most one row. Ownership is checked on the way out, which is
// equivalent here and leaks nothing.
func (s *TenantStore) FindTargetByAddress(ctx context.Context, hostname string, port int) (*state.Target, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	t, err := s.base.FindTargetByAddress(ctx, hostname, port)
	if err != nil || t == nil {
		return nil, err
	}
	if t.TenantID != tid {
		return nil, nil
	}
	return t, nil
}

func (s *TenantStore) ListTargets(ctx context.Context, filter state.TargetFilter) ([]*state.Target, error) {
	_, tid, err := s.scope(ctx)
	if err != nil {
		return nil, err
	}
	filter.TenantID = tid
	return s.base.ListTargets(ctx, filter)
}

func (s *TenantStore) UpdateTargetStatus(ctx context.Context, id, status string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	t, err := s.base.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	if t != nil && t.TenantID != tid {
		return fmt.Errorf("%w: target %q belongs to tenant %q", ErrCrossTenantAccess, id, t.TenantID)
	}
	return s.base.UpdateTargetStatus(ctx, id, status)
}

func (s *TenantStore) SetTargetReachability(ctx context.Context, id string, reachable bool, at time.Time) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	t, err := s.base.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	if t != nil && t.TenantID != tid {
		return fmt.Errorf("%w: target %q belongs to tenant %q", ErrCrossTenantAccess, id, t.TenantID)
	}
	return s.base.SetTargetReachability(ctx, id, reachable, at)
}

func (s *TenantStore) DeleteTarget(ctx context.Context, id string) error {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return err
	}
	t, err := s.base.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	if t != nil && t.TenantID != tid {
		return fmt.Errorf("%w: target %q belongs to tenant %q", ErrCrossTenantAccess, id, t.TenantID)
	}
	return s.base.DeleteTarget(ctx, id)
}

// CountTargetsInGroup counts rows, so it is scoped by checking the group
// belongs to the caller and then counting only that tenant's targets.
//
// The base method has no filter parameter, so it cannot express the predicate
// in SQL. Returning 0 rather than an error for a foreign group keeps the
// method's contract (a count, not an existence check) while revealing nothing:
// "0 targets" and "no such group for you" are the same answer to a caller that
// was never entitled to either.
func (s *TenantStore) CountTargetsInGroup(ctx context.Context, groupID string) (int, error) {
	ctx, tid, err := s.scope(ctx)
	if err != nil {
		return 0, err
	}
	g, err := s.base.GetInventoryGroup(ctx, groupID)
	if err != nil {
		return 0, err
	}
	if g == nil || g.TenantID != tid {
		return 0, nil
	}
	n, err := s.base.CountTargetsInGroup(ctx, groupID)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	// The base count is table-wide; recount from the tenant-scoped list so
	// the number actually reflects the caller's own targets.
	targets, err := s.base.ListTargets(ctx, state.TargetFilter{GroupID: groupID, TenantID: tid})
	if err != nil {
		return 0, err
	}
	return len(targets), nil
}

// --- Close ------------------------------------------------------------------

func (s *TenantStore) Close() error {
	if s == nil || s.base == nil {
		return nil
	}
	return s.base.Close()
}
