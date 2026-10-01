package tenant

// verify.go is the self-check for tenant isolation.
//
// It exists because the failure mode this package guards against is silent: a
// missing predicate does not crash, it just returns another tenant's rows. A
// self-check that only runs in CI would not catch a regression introduced by a
// deployment-side configuration change, and one that only runs in tests would
// not catch a base-store method that was added later. VerifyIsolation is
// written to be callable from either place.

import (
	"context"
	"fmt"

	"github.com/nexus/levee/internal/state"
)

// VerifyIsolation asserts that two tenants cannot observe each other's data
// through the given store. tenantA and tenantB must be distinct, non-empty,
// and must each already own at least one row; the function creates no data.
//
// It is a POSITIVE check, not a proof: it can only test the tenants and the
// entity types it is pointed at. Its value is that it exercises the real Store
// surface — every method below is a genuine isolation boundary, so a wrapper
// that forgot to scope one of them shows up as a failure rather than as a
// latent leak.
func VerifyIsolation(ctx context.Context, store state.Store, tenantA, tenantB string) error {
	if store == nil {
		return fmt.Errorf("%w: nil store", ErrIsolationViolation)
	}
	if tenantA == "" || tenantB == "" || tenantA == tenantB {
		return fmt.Errorf("%w: need two distinct non-empty tenant ids", ErrIsolationViolation)
	}

	ctxA := ContextWithTenant(ctx, tenantA)
	ctxB := ContextWithTenant(ctx, tenantB)

	runsA, err := store.ListRuns(ctxA, state.RunFilter{})
	if err != nil {
		return fmt.Errorf("%w: list runs for tenant A: %v", ErrIsolationViolation, err)
	}
	runsB, err := store.ListRuns(ctxB, state.RunFilter{})
	if err != nil {
		return fmt.Errorf("%w: list runs for tenant B: %v", ErrIsolationViolation, err)
	}

	// 1. The two listings must be disjoint.
	idsA := make(map[string]struct{}, len(runsA))
	for _, r := range runsA {
		idsA[r.ID] = struct{}{}
	}
	for _, r := range runsB {
		if _, ok := idsA[r.ID]; ok {
			return fmt.Errorf("%w: run %s visible to both tenants", ErrIsolationViolation, r.ID)
		}
	}

	// 2. Cross-tenant read by id must look like a miss, not an error and not a
	//    row. Returning an error would let an attacker distinguish "exists but
	//    not yours" from "does not exist", which is an enumeration oracle.
	for _, r := range runsB {
		got, err := store.GetRun(ctxA, r.ID)
		if err != nil {
			return fmt.Errorf("%w: cross-tenant GetRun returned an error (want a miss): %v", ErrIsolationViolation, err)
		}
		if got != nil {
			return fmt.Errorf("%w: tenant A fetched tenant B's run %s", ErrIsolationViolation, r.ID)
		}
	}

	// 3. Cross-tenant mutation must be refused, not silently applied.
	for _, r := range runsB {
		mutated := *r
		mutated.Status = "completed"
		if err := store.UpdateRun(ctxA, &mutated); err == nil {
			return fmt.Errorf("%w: tenant A updated tenant B's run %s", ErrIsolationViolation, r.ID)
		}
		// And the row must be unchanged afterwards.
		after, err := store.GetRun(ctxB, r.ID)
		if err != nil {
			return fmt.Errorf("%w: re-read run %s: %v", ErrIsolationViolation, r.ID, err)
		}
		if after == nil || after.Status != r.Status {
			return fmt.Errorf("%w: cross-tenant UpdateRun mutated run %s", ErrIsolationViolation, r.ID)
		}
	}

	// 4. Every run-scoped child list must be empty for the other tenant.
	if err := expectEmpty(ctxA, store, "ListBatches", func(c context.Context) (int, error) {
		out, err := store.ListBatches(c, state.BatchFilter{})
		return len(out), err
	}); err != nil {
		return err
	}
	if err := expectEmpty(ctxA, store, "ListSteps", func(c context.Context) (int, error) {
		out, err := store.ListSteps(c, state.StepFilter{})
		return len(out), err
	}); err != nil {
		return err
	}
	if err := expectEmpty(ctxA, store, "ListTraces", func(c context.Context) (int, error) {
		out, err := store.ListTraces(c, state.TraceFilter{})
		return len(out), err
	}); err != nil {
		return err
	}
	if err := expectEmpty(ctxA, store, "ListApprovals", func(c context.Context) (int, error) {
		out, err := store.ListApprovals(c, state.ApprovalFilter{})
		return len(out), err
	}); err != nil {
		return err
	}

	// 5. Credentials are the highest-consequence leak — they are the keys to
	//    the machines. Assert them explicitly rather than assuming the run
	//    scoped checks generalise.
	credsA, err := store.ListCredentials(ctxA)
	if err != nil {
		return fmt.Errorf("%w: list credentials for tenant A: %v", ErrIsolationViolation, err)
	}
	credsB, err := store.ListCredentials(ctxB)
	if err != nil {
		return fmt.Errorf("%w: list credentials for tenant B: %v", ErrIsolationViolation, err)
	}
	byID := make(map[string]string, len(credsB))
	for _, c := range credsB {
		byID[c.ID] = c.TenantID
	}
	for _, c := range credsA {
		if owner, ok := byID[c.ID]; ok {
			return fmt.Errorf("%w: credential %s visible to both tenants (owners %q and %q)",
				ErrIsolationViolation, c.ID, c.TenantID, owner)
		}
	}
	for _, c := range credsB {
		got, err := store.GetCredential(ctxA, c.ID)
		if err != nil {
			return fmt.Errorf("%w: cross-tenant GetCredential returned an error (want a miss): %v", ErrIsolationViolation, err)
		}
		if got != nil {
			return fmt.Errorf("%w: tenant A fetched tenant B's credential %s", ErrIsolationViolation, c.ID)
		}
	}

	return nil
}

// expectEmpty asserts that a run-scoped child listing issued on behalf of
// tenant A returns no rows at all. A non-zero count means the predicate was
// not applied, which is the exact regression this self-check exists to catch.
func expectEmpty(ctx context.Context, _ state.Store, name string, list func(context.Context) (int, error)) error {
	n, err := list(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrIsolationViolation, name, err)
	}
	if n != 0 {
		return fmt.Errorf("%w: tenant A sees %d row(s) via %s, expected none", ErrIsolationViolation, n, name)
	}
	return nil
}
