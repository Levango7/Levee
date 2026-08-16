// Package tenant implements multi-tenancy support for LEVEE.
//
// It provides tenant isolation (row-level data isolation plus namespace
// scoping), resource quotas (max targets, concurrent changes, storage, API
// rate), tenant lifecycle management (create / suspend / resume / delete),
// and a CLI surface for managing tenants.
//
// The package is organised around four core types:
//
//   - Tenant: the tenant identity and metadata.
//   - Quota / Usage / QuotaManager: per-tenant resource limits and current
//     consumption tracking.
//   - IsolatedStore: a wrapper over state.Store that injects tenant_id
//     filtering so that one tenant cannot observe another tenant's data.
//   - TenantManager: the high-level orchestrator that ties tenants, quotas
//     and isolation together and exposes lifecycle operations.
//
// All shared mutable state is guarded by sync.RWMutex. The package never
// panics; errors are returned with the fmt.Errorf("xxx: %w", err) wrapping
// convention used throughout LEVEE.
package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// TenantStatus represents the lifecycle state of a tenant.
type TenantStatus int

const (
	// TenantActive means the tenant is operational and may perform actions.
	TenantActive TenantStatus = iota
	// TenantSuspended means the tenant is paused; actions are rejected
	// until the tenant is resumed.
	TenantSuspended
	// TenantDeleted marks a soft-deleted tenant. The record is retained
	// for audit but all operations are rejected.
	TenantDeleted
)

// String returns the human-readable name of the status. It is used in
// serialisation, logging and CLI output.
func (s TenantStatus) String() string {
	switch s {
	case TenantActive:
		return "active"
	case TenantSuspended:
		return "suspended"
	case TenantDeleted:
		return "deleted"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// ParseTenantStatus converts a string to a TenantStatus. Unknown values
// return an error so that callers cannot silently mistype a status.
func ParseTenantStatus(s string) (TenantStatus, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return TenantActive, nil
	case "suspended":
		return TenantSuspended, nil
	case "deleted":
		return TenantDeleted, nil
	default:
		return 0, fmt.Errorf("tenant: unknown status %q", s)
	}
}

// Sentinel errors returned by tenant operations. Callers should test against
// these with errors.Is rather than string matching.
var (
	// ErrTenantNotFound is returned when a tenant lookup yields no result.
	ErrTenantNotFound = errors.New("tenant: not found")
	// ErrTenantExists is returned when creating a tenant whose name is
	// already in use.
	ErrTenantExists = errors.New("tenant: already exists")
	// ErrTenantSuspended is returned when an operation is attempted on a
	// suspended tenant.
	ErrTenantSuspended = errors.New("tenant: suspended")
	// ErrTenantDeleted is returned when an operation is attempted on a
	// soft-deleted tenant.
	ErrTenantDeleted = errors.New("tenant: deleted")
	// ErrInvalidTenant is returned when a tenant field fails validation.
	ErrInvalidTenant = errors.New("tenant: invalid")
)

// namePattern restricts tenant names to lowercase letters, digits and
// hyphens, matching the Kubernetes namespace naming convention so that
// tenant names can be safely embedded in DNS-1123 labels.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}[a-z0-9]$`)

// Tenant is the core tenant identity. A Tenant owns runs, batches, steps,
// traces, approvals and audits; the IsolatedStore enforces that no tenant
// can observe another tenant's data.
type Tenant struct {
	// ID is the immutable unique identifier of the tenant. It is generated
	// by NewTenant and never changes.
	ID string `json:"id"`
	// Name is the unique, human-friendly name of the tenant. It must match
	// the DNS-1123 label convention (lowercase alphanumerics and hyphens).
	Name string `json:"name"`
	// DisplayName is a free-form, human-readable label shown in UIs and
	// CLI output. It is not required to be unique.
	DisplayName string `json:"display_name"`
	// Namespace is the per-tenant namespace prefix used when scoping
	// resources (e.g. "tenant-acme"). It is derived from Name.
	Namespace string `json:"namespace"`
	// Status is the current lifecycle state of the tenant.
	Status TenantStatus `json:"status"`
	// CreatedAt is the time the tenant was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is the time the tenant was last modified.
	UpdatedAt time.Time `json:"updated_at"`
	// Labels is an optional set of user-supplied key/value pairs for
	// arbitrary metadata (e.g. owner, cost-center).
	Labels map[string]string `json:"labels,omitempty"`
}

// NewTenant creates a new Tenant with a generated ID, Active status and
// the current timestamp. It does not validate the name; callers should
// call Validate before persisting the tenant.
func NewTenant(name, displayName string) *Tenant {
	now := time.Now().UTC()
	return &Tenant{
		ID:          newTenantID(),
		Name:        name,
		DisplayName: displayName,
		Namespace:   NamespaceFor(name),
		Status:      TenantActive,
		CreatedAt:   now,
		UpdatedAt:   now,
		Labels:      make(map[string]string),
	}
}

// newTenantID generates a unique tenant identifier using crypto/rand. The
// ID has the form "tenant-<16-hex-chars>". On the extremely unlikely event
// that rand.Read fails, it falls back to a timestamp-based ID so the
// caller always gets a usable, unique-enough identifier.
func newTenantID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("tenant-%d", time.Now().UnixNano())
	}
	return "tenant-" + hex.EncodeToString(b)
}

// NamespaceFor returns the per-tenant namespace prefix for a given name.
// The namespace is "tenant-<name>"; it is used to scope resources that
// must be unique per tenant (e.g. lock scopes, template names).
func NamespaceFor(name string) string {
	return "tenant-" + name
}

// Validate checks that the tenant has a valid name, non-empty ID and a
// consistent namespace. It returns ErrInvalidTenant with a descriptive
// message when validation fails.
func (t *Tenant) Validate() error {
	if t == nil {
		return fmt.Errorf("%w: nil tenant", ErrInvalidTenant)
	}
	if t.ID == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidTenant)
	}
	if t.Name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidTenant)
	}
	if !namePattern.MatchString(t.Name) {
		return fmt.Errorf("%w: name %q must match %s", ErrInvalidTenant, t.Name, namePattern.String())
	}
	if t.Namespace == "" {
		return fmt.Errorf("%w: empty namespace", ErrInvalidTenant)
	}
	if t.Namespace != NamespaceFor(t.Name) {
		return fmt.Errorf("%w: namespace %q does not match name %q", ErrInvalidTenant, t.Namespace, t.Name)
	}
	return nil
}

// IsOperational reports whether the tenant can perform actions. A tenant
// is operational only when its status is Active.
func (t *Tenant) IsOperational() bool {
	if t == nil {
		return false
	}
	return t.Status == TenantActive
}

// --- Context propagation ---------------------------------------------------

// ctxKey is an unexported context key type so that callers cannot collide
// with this package's context values.
type ctxKey int

const (
	// tenantCtxKey holds the tenant ID string in a context.
	tenantCtxKey ctxKey = iota
)

// ContextWithTenant returns a new context that carries the given tenant ID.
// It is the canonical way to propagate the active tenant through a request
// pipeline without modifying function signatures.
func ContextWithTenant(ctx context.Context, tenantID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, tenantCtxKey, tenantID)
}

// TenantFromContext extracts the tenant ID from the context. It returns
// ErrTenantNotFound when no tenant is bound to the context, so callers
// can distinguish "no tenant" from "empty tenant".
func TenantFromContext(ctx context.Context) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("%w: nil context", ErrTenantNotFound)
	}
	v := ctx.Value(tenantCtxKey)
	if v == nil {
		return "", fmt.Errorf("%w: no tenant in context", ErrTenantNotFound)
	}
	id, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: tenant context value is not a string", ErrTenantNotFound)
	}
	if id == "" {
		return "", fmt.Errorf("%w: empty tenant id in context", ErrTenantNotFound)
	}
	return id, nil
}

// MustTenantFromContext is a convenience wrapper for tests and internal
// call sites that have already validated the tenant upstream. It panics
// when the context does not carry a tenant; production code should use
// TenantFromContext instead.
func MustTenantFromContext(ctx context.Context) string {
	id, err := TenantFromContext(ctx)
	if err != nil {
		panic(err)
	}
	return id
}