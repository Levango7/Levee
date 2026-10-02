package tenant

// store_test.go proves the TenantStore actually enforces isolation.
//
// The state package's tests prove the SQL predicates exist; these prove the
// wrapper SETS them, refuses cross-tenant writes, and fails closed. A wrapper
// that compiles but silently passes the wrong tenant is the failure mode that
// would not be caught by anything else in the suite, so each test below drives
// a real SQLite store through the wrapper rather than a mock.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTenantStore(t *testing.T, enabled bool) (*TenantStore, state.Store) {
	t.Helper()
	ctx := context.Background()
	base, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "tenant-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	return NewTenantStore(base, NewResolver(enabled), nil), base
}

func asACME(ctx context.Context) context.Context { return ContextWithTenant(ctx, "acme") }
func asGlobex(ctx context.Context) context.Context {
	return ContextWithTenant(ctx, "globex")
}

func seedRun(t *testing.T, s *TenantStore, ctx context.Context, id, name string) {
	t.Helper()
	require.NoError(t, s.CreateRun(ctx, &state.Run{
		ID: id, WorkflowName: name, TemplateName: "tpl", PlanHash: "h",
		Status: "draft", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Creator: "alice",
	}))
}

func TestTenantStore_RunsAreInvisibleAcrossTenants(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	seedRun(t, s, asACME(ctx), "r-acme", "wf-a")
	seedRun(t, s, asGlobex(ctx), "r-globex", "wf-g")

	acmeRuns, err := s.ListRuns(asACME(ctx), state.RunFilter{})
	require.NoError(t, err)
	require.Len(t, acmeRuns, 1)
	assert.Equal(t, "r-acme", acmeRuns[0].ID)
	assert.Equal(t, "acme", acmeRuns[0].TenantID, "create must stamp the resolved tenant")

	// A caller-supplied TenantID must not widen the scope the resolver
	// decided. Asking for globex while acting as acme must return ACME's rows,
	// not globex's and not nothing — the resolver's value overwrites the
	// caller's, so the caller can neither escalate nor accidentally blind
	// itself.
	forged, err := s.ListRuns(asACME(ctx), state.RunFilter{TenantID: "globex"})
	require.NoError(t, err)
	require.Len(t, forged, 1)
	assert.Equal(t, "r-acme", forged[0].ID,
		"the resolved tenant must win over a caller-supplied filter value")

	// Cross-tenant read by id must be a MISS, not an error and not a row.
	// Returning an error would be an enumeration oracle: it would let a
	// caller distinguish "exists but not yours" from "does not exist".
	got, err := s.GetRun(asACME(ctx), "r-globex")
	require.NoError(t, err, "cross-tenant GetRun must not error")
	assert.Nil(t, got, "cross-tenant GetRun must look like a miss")

	own, err := s.GetRun(asACME(ctx), "r-acme")
	require.NoError(t, err)
	require.NotNil(t, own)
}

func TestTenantStore_CrossTenantWriteIsRefusedAndRowUnchanged(t *testing.T) {
	ctx := context.Background()
	s, base := newTenantStore(t, true)

	seedRun(t, s, asGlobex(ctx), "r-globex", "wf-g")

	// A caller cannot move a run into its own tenant by setting the field on
	// the struct it passes in: UpdateRun re-stamps from the resolved tenant.
	stolen, err := s.GetRun(asGlobex(ctx), "r-globex")
	require.NoError(t, err)
	require.NotNil(t, stolen)

	attacker := *stolen
	attacker.TenantID = "acme"
	attacker.Status = "completed"
	err = s.UpdateRun(asACME(ctx), &attacker)
	require.Error(t, err, "tenant A must not be able to update tenant B's run")
	assert.True(t, errors.Is(err, ErrCrossTenantAccess), "got: %v", err)

	// Verified against the BASE store, so a bug in the wrapper's own read path
	// cannot mask the failure.
	after, err := base.GetRun(ctx, "r-globex")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "draft", after.Status, "the row must be untouched")
	assert.Equal(t, "globex", after.TenantID, "ownership must not have moved")

	// Delete must be refused too.
	require.Error(t, s.DeleteRun(asACME(ctx), "r-globex"))
	stillThere, err := base.GetRun(ctx, "r-globex")
	require.NoError(t, err)
	assert.NotNil(t, stillThere)
}

func TestTenantStore_ChildRowsCannotBeInjectedIntoForeignRun(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	seedRun(t, s, asGlobex(ctx), "r-globex", "wf-g")
	seedRun(t, s, asACME(ctx), "r-acme", "wf-a")

	// A step written into another tenant's run would be stamped acme while its
	// parent belongs to globex, breaking the child/parent agreement and hiding
	// a real execution record from the run's owner.
	err := s.CreateStep(asACME(ctx), &state.Step{
		ID: "s-evil", RunID: "r-globex", BatchID: "b-x", Host: "h1",
		StepName: "reload", Action: "shell.exec", Status: "success",
	})
	require.Error(t, err, "must not create a child row inside a foreign run")
	assert.True(t, errors.Is(err, ErrCrossTenantAccess), "got: %v", err)

	err = s.CreateBatch(asACME(ctx), &state.Batch{ID: "b-evil", RunID: "r-globex", BatchNo: 1})
	require.Error(t, err)

	err = s.CreateApproval(asACME(ctx), &state.Approval{
		ID: "a-evil", RunID: "r-globex", Level: "high", Approver: "bob", Status: "pending",
	})
	require.Error(t, err)

	// The legitimate path still works.
	require.NoError(t, s.CreateBatch(asACME(ctx), &state.Batch{ID: "b-ok", RunID: "r-acme", BatchNo: 1}))

	// Globex's own view is untouched.
	globexSteps, err := s.ListSteps(asGlobex(ctx), state.StepFilter{})
	require.NoError(t, err)
	assert.Empty(t, globexSteps)
}

func TestTenantStore_CredentialsAreIsolated(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	mk := func(c context.Context, id string) {
		require.NoError(t, s.CreateCredential(c, &state.Credential{
			ID: id, Name: "key-" + id, Type: "ssh_key",
			EncryptedData: []byte{0x00}, CreatedAt: time.Now().UTC(),
		}))
	}
	mk(asACME(ctx), "c-acme")
	mk(asGlobex(ctx), "c-globex")

	// ListCredentials has no filter parameter in the base store, so the wrapper
	// filters in memory. This is the assertion that makes that acceptable.
	acmeCreds, err := s.ListCredentials(asACME(ctx))
	require.NoError(t, err)
	require.Len(t, acmeCreds, 1, "a tenant must not see another tenant's credential keys")
	assert.Equal(t, "c-acme", acmeCreds[0].ID)

	got, err := s.GetCredential(asACME(ctx), "c-globex")
	require.NoError(t, err)
	assert.Nil(t, got, "cross-tenant credential read must be a miss")

	// Name lookup is table-wide UNIQUE, so it is ownership-checked on the way
	// out instead.
	byName, err := s.GetCredentialByName(asACME(ctx), "key-c-globex")
	require.NoError(t, err)
	assert.Nil(t, byName)

	require.Error(t, s.DeleteCredential(asACME(ctx), "c-globex"))
}

func TestTenantStore_FailsClosedWithoutTenantInContext(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	seedRun(t, s, asACME(ctx), "r-acme", "wf-a")

	// Multi-tenancy is ON and the caller carries no tenant. Every scoped
	// operation must fail rather than silently reading the default tenant's
	// data — that fallback is precisely the bug this policy exists to prevent.
	_, err := s.ListRuns(ctx, state.RunFilter{})
	require.Error(t, err, "a context with no tenant must be refused, not defaulted")
	assert.True(t, errors.Is(err, ErrTenantNotFound), "got: %v", err)

	_, err = s.GetRun(ctx, "r-acme")
	require.Error(t, err)

	err = s.CreateRun(ctx, &state.Run{ID: "r-new", WorkflowName: "w", TemplateName: "t",
		PlanHash: "h", Status: "draft", CreatedAt: time.Now(), UpdatedAt: time.Now(), Creator: "x"})
	require.Error(t, err)
}

// TestTenantStore_DisabledModeIsUnchanged is the back-compat guarantee: with
// multi-tenancy off, the wrapper is a pass-through into the default tenant and
// an un-scoped context works exactly as it did before this package shipped.
func TestTenantStore_DisabledModeIsUnchanged(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, false)

	// No tenant in the context at all — this is the pre-multi-tenancy call
	// shape and it must not error.
	require.NoError(t, s.CreateRun(ctx, &state.Run{
		ID: "r-1", WorkflowName: "w", TemplateName: "t", PlanHash: "h", Status: "draft",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Creator: "x",
	}))

	runs, err := s.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, DefaultTenantID, runs[0].TenantID,
		"single-tenant rows must be owned by the default tenant")

	// And a context that DOES carry a tenant is ignored while disabled, so an
	// operator cannot accidentally create split-brain data before enabling the
	// feature.
	seedRun(t, s, asACME(ctx), "r-2", "w")
	all, err := s.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err)
	assert.Len(t, all, 2, "a disabled deployment must not split rows by context tenant")
}

func TestTenantStore_LocksRemainGlobal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	seedRun(t, s, asACME(ctx), "r-acme", "w")

	require.NoError(t, s.CreateLock(asACME(ctx), &state.Lock{
		ID: "l-1", Scope: "host:db-1", Owner: "r-acme", TTLSeconds: 60,
		AcquiredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	}))

	// Tenant B taking the same host must collide, not get its own row. If
	// locks were tenant-scoped this would succeed and two tenants would be
	// admitted to change one machine at the same time.
	err := s.CreateLock(asGlobex(ctx), &state.Lock{
		ID: "l-2", Scope: "host:db-1", Owner: "r-globex", TTLSeconds: 60,
		AcquiredAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
	})
	require.Error(t, err, "host locks must stay cross-tenant")

	locks, err := s.ListLocks(asGlobex(ctx))
	require.NoError(t, err)
	assert.Len(t, locks, 1, "the blocked acquire must not have created a second row")
}

func TestTenantStore_ClusterViewsStayPlatformScoped(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	// A single-node SQLite deployment has no cluster_nodes table; the contract
	// is (nil, nil). The point of the test is that the wrapper does not inject
	// a tenant predicate into these platform views at all.
	nodes, err := s.ListClusterNodes(asACME(ctx))
	require.NoError(t, err)
	assert.Empty(t, nodes)

	summary, err := s.AssignmentSummary(asACME(ctx))
	require.NoError(t, err)
	if summary != nil {
		assert.Equal(t, 0, summary.TotalActive)
	}
}

func TestTenantStore_VerifyIsolationSelfCheck(t *testing.T) {
	ctx := context.Background()
	s, _ := newTenantStore(t, true)

	seedRun(t, s, asACME(ctx), "r-acme", "wf-a")
	seedRun(t, s, asGlobex(ctx), "r-globex", "wf-g")
	require.NoError(t, s.CreateCredential(asACME(ctx), &state.Credential{
		ID: "c-a", Name: "key-a", Type: "ssh_key", EncryptedData: []byte{1}, CreatedAt: time.Now(),
	}))
	require.NoError(t, s.CreateCredential(asGlobex(ctx), &state.Credential{
		ID: "c-g", Name: "key-g", Type: "ssh_key", EncryptedData: []byte{2}, CreatedAt: time.Now(),
	}))

	require.NoError(t, VerifyIsolation(ctx, s, "acme", "globex"))

	// And it must actually fail when isolation IS broken — otherwise it is a
	// self-check that always passes. This is done by pointing the same check at
	// the base store, which has no isolation.
	base := s.Base()
	err := VerifyIsolation(ctx, base, "acme", "globex")
	require.Error(t, err, "the self-check must detect an unscoped store, not pass vacuously")
}

// TestTenantStore_IsADropInReplacement is the property the old IsolatedStore
// could not offer: the wrapper is usable anywhere a Store is. The compile-time
// assertion in store.go enforces the method set; this test enforces that the
// value actually satisfies the interface at runtime too, and that a nil
// resolver degrades to single-tenant rather than panicking.
func TestTenantStore_IsADropInReplacement(t *testing.T) {
	ctx := context.Background()
	base, err := state.NewSQLiteStore(ctx, filepath.Join(t.TempDir(), "dropin.db"))
	require.NoError(t, err)
	defer func() { _ = base.Close() }()

	var _ state.Store = NewTenantStore(base, nil, nil)

	s := NewTenantStore(base, nil, nil)
	require.NoError(t, s.CreateRun(ctx, &state.Run{
		ID: "r-1", WorkflowName: "w", TemplateName: "t", PlanHash: "h", Status: "draft",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), Creator: "x",
	}))
	runs, err := s.ListRuns(ctx, state.RunFilter{})
	require.NoError(t, err, "a nil resolver must mean single-tenant, not a panic")
	assert.Len(t, runs, 1)
}
