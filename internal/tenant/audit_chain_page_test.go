package tenant

// TenantStore's audit chain walk. The chain spans whichever rows the Store
// exposes, so a walk that ignored the tenant would chain one tenant's audit rows
// onto another's — a leak of another tenant's row contents into this tenant's
// hashes, and a chain that fails verification for reasons that look like
// tampering. This file pins that the walk is scoped, and that the scope comes
// from the resolved context rather than from the caller's argument.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestTenantStore_AuditChainPageIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)
	base := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < 3; i++ {
		ts := base.Add(time.Duration(i) * time.Second)
		require.NoError(t, s.CreateAudit(asACME(ctx), &state.Audit{
			ID: acmeAuditID(i), Action: "login", Actor: "alice", Result: "success", Timestamp: ts,
		}))
		require.NoError(t, s.CreateAudit(asGlobex(ctx), &state.Audit{
			ID: globexAuditID(i), Action: "login", Actor: "bob", Result: "success", Timestamp: ts,
		}))
	}

	acmePage, err := s.ListAuditChainPage(asACME(ctx), nil, 100, "")
	require.NoError(t, err)
	require.Len(t, acmePage, 3)
	for _, a := range acmePage {
		assert.Equal(t, "acme", a.TenantID, "a tenant's chain must span its own rows only")
	}

	globexPage, err := s.ListAuditChainPage(asGlobex(ctx), nil, 100, "")
	require.NoError(t, err)
	require.Len(t, globexPage, 3)
	for _, a := range globexPage {
		assert.Equal(t, "globex", a.TenantID)
	}
}

// TestTenantStore_AuditChainPageIgnoresCallerSuppliedTenant pins the scope to the
// context. ListAuditChainPage takes a tenantID argument for the unscoped
// backends; through TenantStore a caller must not be able to name someone else
// and read their chain through it.
func TestTenantStore_AuditChainPageIgnoresCallerSuppliedTenant(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	require.NoError(t, s.CreateAudit(asACME(ctx), &state.Audit{
		ID: "acme-only", Action: "login", Actor: "alice", Result: "success",
		Timestamp: time.Now().UTC(),
	}))
	require.NoError(t, s.CreateAudit(asGlobex(ctx), &state.Audit{
		ID: "globex-only", Action: "login", Actor: "bob", Result: "success",
		Timestamp: time.Now().UTC().Add(time.Millisecond),
	}))

	page, err := s.ListAuditChainPage(asACME(ctx), nil, 100, "globex")
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, "acme-only", page[0].ID,
		"the caller-supplied tenant must not widen the scope the context resolved")
}

// TestTenantStore_AuditChainPageRequiresTenantContext checks the walk fails
// closed: with no tenant in context there is nothing to scope to, so it must
// error rather than fall back to an unscoped read.
func TestTenantStore_AuditChainPageRequiresTenantContext(t *testing.T) {
	s, _ := newTenantStore(t, true)

	_, err := s.ListAuditChainPage(context.Background(), nil, 100, "")
	require.Error(t, err)
}

func acmeAuditID(i int) string   { return "acme-audit-" + string(rune('a'+i)) }
func globexAuditID(i int) string { return "globex-audit-" + string(rune('a'+i)) }

// TestTenantStore_UpdateTraceChainIsTenantScoped covers the seal's write
// boundary: a tenant must not be able to stamp chain hashes onto another
// tenant's trace. Getting this wrong would not leak row contents, but it would
// let one tenant's seal corrupt another tenant's chain — and because the chain
// is exactly what verification trusts, that is a silent integrity break.
func TestTenantStore_UpdateTraceChainIsTenantScoped(t *testing.T) {
	ctx := context.Background()
	s, base := newTenantStore(t, true)
	ts := time.Now().UTC()

	// Traces hang off runs (FK), so both tenants' runs must exist first.
	seedRun(t, s, asACME(ctx), "acme-run", "wf-a")
	seedRun(t, s, asGlobex(ctx), "globex-run", "wf-b")

	require.NoError(t, base.CreateTrace(ctx, &state.Trace{
		ID: "acme-trace", RunID: "acme-run", Event: "step_execute", Actor: "alice",
		Detail: `{}`, Timestamp: ts, TenantID: "acme",
	}))
	require.NoError(t, base.CreateTrace(ctx, &state.Trace{
		ID: "globex-trace", RunID: "globex-run", Event: "step_execute", Actor: "bob",
		Detail: `{}`, Timestamp: ts.Add(time.Second), TenantID: "globex",
	}))

	// Own tenant: the seal's write goes through.
	require.NoError(t, s.UpdateTraceChain(asACME(ctx), "acme-trace", "p", "c"))

	// Another tenant's row: refused, even though the row id is known.
	err := s.UpdateTraceChain(asACME(ctx), "globex-trace", "p", "c")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCrossTenantAccess)

	untouched, err := base.GetTrace(ctx, "globex-trace")
	require.NoError(t, err)
	assert.Empty(t, untouched.CurrHash, "the other tenant's chain must be untouched")
}
