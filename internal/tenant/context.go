package tenant

// context.go adds the request-scoping POLICY on top of the context plumbing
// that tenant.go already provides.
//
// The distinction matters: tenant.go's ContextWithTenant / TenantFromContext
// are a pure transport — they put a string in a context and get it back, and
// they have been in the package (and tested) all along with no production
// caller. What was missing is the decision of WHICH tenant a given call runs
// as, and in particular what to do when the context carries none. That
// decision is a safety property, not a convenience, so it lives here rather
// than being spread across call sites that each re-derive it.
//
// WHY A CONTEXT AND NOT A TENANT FIELD ON THE STORE: every method on
// state.Store already takes a context.Context as its first argument. That one
// property is what makes per-request isolation possible without threading a
// tenant parameter through ~80 method signatures, and it is why a single store
// instance can serve every tenant: the tenant is a property of the CALL, not
// of the object. That is what lets TenantStore be a real drop-in replacement
// for state.Store rather than a per-tenant construction.
//
// WHY THE TENANT COMES FROM THE AUTHENTICATED IDENTITY, NOT A HEADER: the
// value is injected by the auth layer from the credential that was actually
// verified (a named token, a session claim, an OIDC claim). A client-supplied
// X-Tenant-Id header would be self-asserted and therefore worthless as an
// isolation boundary.

import (
	"context"
	"errors"

	"github.com/nexus/levee/internal/state"
)

// Sentinel errors callers test with errors.Is rather than string matching.
// They moved here from the old isolation.go when the IncidentID-encoding
// IsolatedStore was replaced by TenantStore; the names and meanings are
// unchanged so existing callers and tests keep working.
var (
	// ErrCrossTenantAccess is returned when a tenant attempts to reach a
	// resource owned by a different tenant.
	ErrCrossTenantAccess = errors.New("tenant: cross-tenant access denied")
	// ErrIsolationViolation is returned when an isolation invariant is
	// violated — for example a write against a run that does not exist, or a
	// child row whose parent run belongs to another tenant.
	ErrIsolationViolation = errors.New("tenant: isolation violation")
)

// DefaultTenantID is the tenant a single-tenant deployment runs as, and the
// owner assigned to rows that predate the tenant_id column. It aliases
// state.DefaultTenantID rather than repeating the literal, so the two cannot
// drift; the migration backfill writes exactly this value.
const DefaultTenantID = state.DefaultTenantID

// Resolver decides which tenant an operation runs as, given the context and
// whether multi-tenancy is enabled for this deployment.
//
// The two modes are deliberately asymmetric:
//
//   - Disabled (the default): the deployment has declared itself single-tenant.
//     Everything resolves to DefaultTenantID. This is what keeps an existing
//     deployment unaffected by the tenant_id columns.
//
//   - Enabled: the context MUST carry a tenant. A missing tenant is
//     ErrTenantNotFound, never a silent default.
//
// That asymmetry is the entire safety argument. If a missing tenant fell back
// to DefaultTenantID while multi-tenancy was ON, then any code path that
// forgot to propagate the context — a new RPC, a background goroutine, a
// future refactor — would silently read and write the default tenant's data.
// A deployment that believes it is isolated but has one unpropagated call site
// is precisely the failure this refuses to allow. Failing closed turns that
// class of bug into a visible error at the first call, rather than a
// cross-tenant read discovered during an incident.
//
// It also means background components (takeover, dispatch, the cluster
// leader) have no free pass: they carry no request context, so under
// multi-tenancy they must explicitly scope themselves with
// ContextWithTenant(ctx, run.TenantID) after loading the run they act on.
type Resolver struct {
	enabled bool
}

// NewResolver returns a Resolver for a deployment. enabled is the operator's
// `tenant.enabled` switch (default false).
func NewResolver(enabled bool) *Resolver { return &Resolver{enabled: enabled} }

// Enabled reports whether multi-tenancy is enforced for this deployment.
func (r *Resolver) Enabled() bool { return r != nil && r.enabled }

// Resolve returns the tenant the current operation runs as.
//
// Disabled -> always DefaultTenantID, whatever the context says.
// Enabled  -> the context's tenant, or ErrTenantNotFound.
func (r *Resolver) Resolve(ctx context.Context) (string, error) {
	if r == nil || !r.enabled {
		return DefaultTenantID, nil
	}
	return TenantFromContext(ctx)
}

// Scope returns a context guaranteed to carry the resolved tenant, plus the
// tenant id. It is the form write paths use: the value a create stamps is then
// by construction the same value a later read filters on.
//
// When multi-tenancy is disabled this still returns a context carrying
// DefaultTenantID rather than the caller's context untouched. That keeps ONE
// code path — callers never branch on whether the feature is on, so there is
// no "the disabled path forgot to stamp the column" variant to get wrong.
//
// A nil Resolver is treated as disabled, matching Resolve, so a
// half-constructed Resolver cannot accidentally enforce isolation or, worse,
// accidentally fail every request.
func (r *Resolver) Scope(ctx context.Context) (context.Context, string, error) {
	id, err := r.Resolve(ctx)
	if err != nil {
		return nil, "", err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return ContextWithTenant(ctx, id), id, nil
}
