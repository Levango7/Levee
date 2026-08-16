package tenant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nexus/levee/internal/log"
	"github.com/nexus/levee/internal/state"
)

// Sentinel errors returned by isolation operations.
var (
	// ErrCrossTenantAccess is returned when a tenant attempts to access
	// a resource owned by a different tenant.
	ErrCrossTenantAccess = errors.New("tenant: cross-tenant access denied")
	// ErrIsolationViolation is returned when an isolation invariant is
	// violated (e.g. a run was created without a tenant_id).
	ErrIsolationViolation = errors.New("tenant: isolation violation")
)

// TenantLabel is the key under which the owning tenant ID is stored in
// run, batch, step, trace and audit records. Because the state.Store
// interface cannot be extended with new columns without modifying
// existing code, the tenant ownership is encoded in the IncidentID
// field of state.Run (which is a free-form string) using the prefix
// "tenant:". This keeps the existing schema and Store interface
// untouched while still allowing per-tenant filtering.
const TenantLabel = "tenant"

// tenantPrefix is the prefix used to embed the tenant id in the
// IncidentID column of state.Run. Using a prefix makes the value
// self-describing and avoids collisions with genuine incident IDs.
const tenantPrefix = "tenant:"

// EncodeTenantTag returns the value to store in state.Run.IncidentID so
// that the run is associated with the given tenant. If the supplied
// incidentID is non-empty it is appended after the tenant tag so that
// the original incident association is preserved.
func EncodeTenantTag(tenantID, incidentID string) string {
	if incidentID == "" {
		return tenantPrefix + tenantID
	}
	return tenantPrefix + tenantID + "|" + incidentID
}

// DecodeTenantTag extracts the tenant ID and the original incident ID
// from a value produced by EncodeTenantTag. It returns ("", "") when
// the value does not carry a tenant tag (i.e. it is a legacy record
// created before multi-tenancy was enabled).
func DecodeTenantTag(incidentID string) (tenantID string, originalIncident string) {
	if !strings.HasPrefix(incidentID, tenantPrefix) {
		return "", incidentID
	}
	rest := incidentID[len(tenantPrefix):]
	if idx := strings.Index(rest, "|"); idx >= 0 {
		return rest[:idx], rest[idx+1:]
	}
	return rest, ""
}

// IsolatedStore wraps a state.Store so that all reads are filtered by
// tenant_id and all writes are tagged with tenant_id. The wrapper does
// not modify the underlying schema; instead it encodes the tenant
// ownership in the IncidentID column of state.Run (see
// EncodeTenantTag) and performs in-memory filtering on read paths.
//
// Because the filtering happens in memory after the base store returns
// results, this wrapper is best suited for moderate result sets. For
// very large deployments a SQL-level WHERE clause would be more
// efficient; that would require extending the Store interface and is
// intentionally left out per the package requirements.
type IsolatedStore struct {
	tenantID string
	base     state.Store
	quota    *QuotaManager
	mu       sync.RWMutex
}

// NewIsolatedStore returns an IsolatedStore bound to the given tenant.
// The quota manager is optional; when non-nil, CreateRun will reserve
// one unit of ResourceConcurrentChanges before delegating to the base
// store.
func NewIsolatedStore(tenantID string, base state.Store, qm *QuotaManager) *IsolatedStore {
	return &IsolatedStore{
		tenantID: tenantID,
		base:     base,
		quota:    qm,
	}
}

// TenantID returns the tenant this store is bound to.
func (s *IsolatedStore) TenantID() string {
	return s.tenantID
}

// CreateRun tags the run with the tenant id and delegates to the base
// store. When a QuotaManager is configured, it reserves one unit of
// ResourceConcurrentChanges; on failure the run is not created.
func (s *IsolatedStore) CreateRun(ctx context.Context, run *state.Run) error {
	if s == nil || s.base == nil {
		return fmt.Errorf("tenant: isolated store not initialised")
	}
	if run == nil {
		return fmt.Errorf("tenant: nil run")
	}

	// Reserve quota before creating the run so that a rejected request
	// does not leave a half-created record.
	if s.quota != nil {
		if err := s.quota.CheckAndReserve(s.tenantID, ResourceConcurrentChanges, 1); err != nil {
			return fmt.Errorf("tenant: reserve concurrent change: %w", err)
		}
	}

	// Tag the run with the tenant id via the IncidentID column.
	tenantIncident := EncodeTenantTag(s.tenantID, run.IncidentID)
	original := run.IncidentID
	run.IncidentID = tenantIncident

	if err := s.base.CreateRun(ctx, run); err != nil {
		// Roll back the reservation on failure.
		if s.quota != nil {
			_ = s.quota.Release(s.tenantID, ResourceConcurrentChanges, 1)
		}
		run.IncidentID = original
		return fmt.Errorf("tenant: create run: %w", err)
	}

	log.DebugCtx(ctx, "tenant run created",
		"tenant_id", s.tenantID, "run_id", run.ID)
	return nil
}

// GetRun returns the run only when it belongs to the bound tenant.
// A run owned by a different tenant returns (nil, nil) so that callers
// cannot distinguish "does not exist" from "belongs to another tenant".
func (s *IsolatedStore) GetRun(ctx context.Context, id string) (*state.Run, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	run, err := s.base.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, nil
	}
	if !s.owns(run) {
		log.WarnCtx(ctx, "cross-tenant get run denied",
			"tenant_id", s.tenantID, "run_id", id)
		return nil, nil
	}
	// Strip the tenant tag from the returned run so callers see the
	// original incident id.
	_, original := DecodeTenantTag(run.IncidentID)
	run.IncidentID = original
	return run, nil
}

// ListRuns returns the subset of runs that belong to the bound tenant
// and match the given filter. The filter is applied by the base store;
// the tenant filter is applied in memory afterwards.
func (s *IsolatedStore) ListRuns(ctx context.Context, filter state.RunFilter) ([]*state.Run, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	runs, err := s.base.ListRuns(ctx, filter)
	if err != nil {
		return nil, err
	}
	result := make([]*state.Run, 0, len(runs))
	for _, r := range runs {
		if s.owns(r) {
			_, original := DecodeTenantTag(r.IncidentID)
			r.IncidentID = original
			result = append(result, r)
		}
	}
	return result, nil
}

// UpdateRun updates the run only when it belongs to the bound tenant.
// The tenant tag is preserved on the stored record.
func (s *IsolatedStore) UpdateRun(ctx context.Context, run *state.Run) error {
	if s == nil || s.base == nil {
		return fmt.Errorf("tenant: isolated store not initialised")
	}
	if run == nil {
		return fmt.Errorf("tenant: nil run")
	}
	existing, err := s.base.GetRun(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("tenant: load run for ownership check: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("%w: run %s not found", ErrCrossTenantAccess, run.ID)
	}
	if !s.owns(existing) {
		return fmt.Errorf("%w: run %s belongs to another tenant",
			ErrCrossTenantAccess, run.ID)
	}
	// Preserve the tenant tag on the updated record.
	_, original := DecodeTenantTag(run.IncidentID)
	run.IncidentID = EncodeTenantTag(s.tenantID, original)
	return s.base.UpdateRun(ctx, run)
}

// DeleteRun deletes the run only when it belongs to the bound tenant.
// On success it releases one unit of ResourceConcurrentChanges when a
// QuotaManager is configured.
func (s *IsolatedStore) DeleteRun(ctx context.Context, id string) error {
	if s == nil || s.base == nil {
		return fmt.Errorf("tenant: isolated store not initialised")
	}
	existing, err := s.base.GetRun(ctx, id)
	if err != nil {
		return fmt.Errorf("tenant: load run for ownership check: %w", err)
	}
	if existing == nil {
		// Nothing to delete; treat as success.
		return nil
	}
	if !s.owns(existing) {
		return fmt.Errorf("%w: run %s belongs to another tenant",
			ErrCrossTenantAccess, id)
	}
	if err := s.base.DeleteRun(ctx, id); err != nil {
		return fmt.Errorf("tenant: delete run: %w", err)
	}
	if s.quota != nil {
		_ = s.quota.Release(s.tenantID, ResourceConcurrentChanges, 1)
	}
	return nil
}

// owns reports whether the given run belongs to the bound tenant.
func (s *IsolatedStore) owns(run *state.Run) bool {
	if run == nil {
		return false
	}
	tid, _ := DecodeTenantTag(run.IncidentID)
	return tid == s.tenantID
}

// --- Trace isolation -------------------------------------------------------

// ListTraces returns the subset of traces whose owning run belongs to
// the bound tenant. Traces do not carry a tenant tag directly; the
// ownership is derived from the parent run's IncidentID column.
func (s *IsolatedStore) ListTraces(ctx context.Context, filter state.TraceFilter) ([]*state.Trace, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	traces, err := s.base.ListTraces(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(traces) == 0 {
		return traces, nil
	}

	// Build the set of run ids that belong to this tenant. We use a
	// targeted ListRuns with a filter narrowed by RunID when possible;
	// otherwise we fall back to listing all runs.
	ownedRunIDs := make(map[string]struct{}, len(traces))
	seenRuns := make(map[string]struct{})
	for _, tr := range traces {
		if _, ok := seenRuns[tr.RunID]; ok {
			continue
		}
		seenRuns[tr.RunID] = struct{}{}
		run, err := s.base.GetRun(ctx, tr.RunID)
		if err != nil || run == nil {
			continue
		}
		if s.owns(run) {
			ownedRunIDs[tr.RunID] = struct{}{}
		}
	}

	result := make([]*state.Trace, 0, len(traces))
	for _, tr := range traces {
		if _, ok := ownedRunIDs[tr.RunID]; ok {
			result = append(result, tr)
		}
	}
	return result, nil
}

// --- Audit isolation -------------------------------------------------------

// ListAudits returns the subset of audit entries whose owning run
// belongs to the bound tenant. The filtering logic mirrors
// ListTraces.
func (s *IsolatedStore) ListAudits(ctx context.Context, filter state.AuditFilter) ([]*state.Audit, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	audits, err := s.base.ListAudits(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(audits) == 0 {
		return audits, nil
	}

	ownedRunIDs := make(map[string]struct{}, len(audits))
	seenRuns := make(map[string]struct{})
	for _, a := range audits {
		if _, ok := seenRuns[a.RunID]; ok {
			continue
		}
		seenRuns[a.RunID] = struct{}{}
		run, err := s.base.GetRun(ctx, a.RunID)
		if err != nil || run == nil {
			continue
		}
		if s.owns(run) {
			ownedRunIDs[a.RunID] = struct{}{}
		}
	}

	result := make([]*state.Audit, 0, len(audits))
	for _, a := range audits {
		if _, ok := ownedRunIDs[a.RunID]; ok {
			result = append(result, a)
		}
	}
	return result, nil
}

// --- Batch / Step isolation ------------------------------------------------

// ListBatches returns the subset of batches whose owning run belongs to
// the bound tenant.
func (s *IsolatedStore) ListBatches(ctx context.Context, filter state.BatchFilter) ([]*state.Batch, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	batches, err := s.base.ListBatches(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(batches) == 0 {
		return batches, nil
	}

	// When the filter narrows by RunID we only need to check that run.
	if filter.RunID != "" {
		run, err := s.base.GetRun(ctx, filter.RunID)
		if err != nil {
			return nil, fmt.Errorf("tenant: load run for ownership check: %w", err)
		}
		if run == nil || !s.owns(run) {
			return nil, nil
		}
		return batches, nil
	}

	// Otherwise filter in memory by run ownership.
	ownedRunIDs := make(map[string]struct{}, len(batches))
	seenRuns := make(map[string]struct{})
	for _, b := range batches {
		if _, ok := seenRuns[b.RunID]; ok {
			continue
		}
		seenRuns[b.RunID] = struct{}{}
		run, err := s.base.GetRun(ctx, b.RunID)
		if err != nil || run == nil {
			continue
		}
		if s.owns(run) {
			ownedRunIDs[b.RunID] = struct{}{}
		}
	}
	result := make([]*state.Batch, 0, len(batches))
	for _, b := range batches {
		if _, ok := ownedRunIDs[b.RunID]; ok {
			result = append(result, b)
		}
	}
	return result, nil
}

// ListSteps returns the subset of steps whose owning run belongs to the
// bound tenant.
func (s *IsolatedStore) ListSteps(ctx context.Context, filter state.StepFilter) ([]*state.Step, error) {
	if s == nil || s.base == nil {
		return nil, fmt.Errorf("tenant: isolated store not initialised")
	}
	steps, err := s.base.ListSteps(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return steps, nil
	}

	if filter.RunID != "" {
		run, err := s.base.GetRun(ctx, filter.RunID)
		if err != nil {
			return nil, fmt.Errorf("tenant: load run for ownership check: %w", err)
		}
		if run == nil || !s.owns(run) {
			return nil, nil
		}
		return steps, nil
	}

	ownedRunIDs := make(map[string]struct{}, len(steps))
	seenRuns := make(map[string]struct{})
	for _, st := range steps {
		if _, ok := seenRuns[st.RunID]; ok {
			continue
		}
		seenRuns[st.RunID] = struct{}{}
		run, err := s.base.GetRun(ctx, st.RunID)
		if err != nil || run == nil {
			continue
		}
		if s.owns(run) {
			ownedRunIDs[st.RunID] = struct{}{}
		}
	}
	result := make([]*state.Step, 0, len(steps))
	for _, st := range steps {
		if _, ok := ownedRunIDs[st.RunID]; ok {
			result = append(result, st)
		}
	}
	return result, nil
}

// --- Verification ----------------------------------------------------------

// VerifyIsolation exercises the isolation invariants for two tenants and
// returns an error when any invariant is violated. It is intended for
// use in tests and as a startup self-check. The two tenants must already
// have at least one run each; the function creates no data.
//
// Invariants checked:
//   - Tenant A cannot see tenant B's runs via ListRuns.
//   - Tenant A cannot fetch tenant B's run via GetRun.
//   - Tenant A cannot list tenant B's traces via ListTraces.
//   - Tenant A cannot list tenant B's audits via ListAudits.
func VerifyIsolation(ctx context.Context, base state.Store, tenantA, tenantB string) error {
	if base == nil {
		return fmt.Errorf("%w: nil base store", ErrIsolationViolation)
	}
	if tenantA == "" || tenantB == "" || tenantA == tenantB {
		return fmt.Errorf("%w: need two distinct non-empty tenant ids", ErrIsolationViolation)
	}

	storeA := NewIsolatedStore(tenantA, base, nil)
	storeB := NewIsolatedStore(tenantB, base, nil)

	runsA, err := storeA.ListRuns(ctx, state.RunFilter{})
	if err != nil {
		return fmt.Errorf("%w: list runs for tenant A: %v", ErrIsolationViolation, err)
	}
	runsB, err := storeB.ListRuns(ctx, state.RunFilter{})
	if err != nil {
		return fmt.Errorf("%w: list runs for tenant B: %v", ErrIsolationViolation, err)
	}

	// Build the set of run ids visible to each tenant and ensure they
	// are disjoint.
	idsA := make(map[string]struct{}, len(runsA))
	for _, r := range runsA {
		idsA[r.ID] = struct{}{}
	}
	for _, r := range runsB {
		if _, ok := idsA[r.ID]; ok {
			return fmt.Errorf("%w: run %s visible to both tenants", ErrIsolationViolation, r.ID)
		}
	}

	// Cross-tenant GetRun must return (nil, nil).
	for _, r := range runsB {
		got, err := storeA.GetRun(ctx, r.ID)
		if err != nil {
			return fmt.Errorf("%w: cross-tenant get returned error: %v", ErrIsolationViolation, err)
		}
		if got != nil {
			return fmt.Errorf("%w: tenant A fetched tenant B run %s", ErrIsolationViolation, r.ID)
		}
	}

	// Cross-tenant trace listing must be empty for runs the other
	// tenant owns.
	tracesA, err := storeA.ListTraces(ctx, state.TraceFilter{})
	if err != nil {
		return fmt.Errorf("%w: list traces for tenant A: %v", ErrIsolationViolation, err)
	}
	for _, tr := range tracesA {
		if _, ok := idsA[tr.RunID]; !ok {
			return fmt.Errorf("%w: tenant A sees trace %s for foreign run %s",
				ErrIsolationViolation, tr.ID, tr.RunID)
		}
	}

	// Cross-tenant audit listing must be empty for foreign runs.
	auditsA, err := storeA.ListAudits(ctx, state.AuditFilter{})
	if err != nil {
		return fmt.Errorf("%w: list audits for tenant A: %v", ErrIsolationViolation, err)
	}
	for _, a := range auditsA {
		if _, ok := idsA[a.RunID]; !ok {
			return fmt.Errorf("%w: tenant A sees audit %s for foreign run %s",
				ErrIsolationViolation, a.ID, a.RunID)
		}
	}

	return nil
}

// TouchUpdatedAt is a small helper used by tests to bump the UpdatedAt
// timestamp of an isolated store without otherwise mutating state. It
// is a no-op on the base store and exists only so that tests can
// simulate concurrent updates.
func (s *IsolatedStore) TouchUpdatedAt(_ time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Intentionally empty: the IsolatedStore has no UpdatedAt field;
	// the method exists so that tests can call it on a typed value.
}
