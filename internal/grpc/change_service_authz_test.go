// change_service_authz_test.go pins the wiring of the policy layer into the
// change-scoped RPCs. The authz package has its own tests for the decision
// rule; what matters here is that a real RPC consults it, refuses with the
// right status, and that a deployment WITHOUT a matrix keeps behaving exactly
// as before (that is the compatibility promise the posture rests on).
package grpc

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// policyFixture: sre may act in dev and only view prod; the operator role
// carries apply/rollback, the viewer role only view.
//
//	alice sre/operator → prod apply allowed VIA ROLE (the case the
//	                     composition exists for)
//	carol sre/viewer   → prod apply denied (reachable, but neither axis
//	                     grants the action)
//	mallory            → denied outright (not registered)
const (
	policyMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
      - name: prod
        actions: [view]
`
	policyRolesYAML = `
roles:
  - name: viewer
    permissions: [view]
  - name: operator
    parent: viewer
    permissions: [apply, rollback]
`
	policyUsersYAML = `
users:
  - name: alice
    team: sre
    role: operator
  - name: carol
    team: sre
    role: viewer
`
)

func newPolicyAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(policyMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.RoleTreeFileName), []byte(policyRolesYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(policyUsersYAML), 0o600))

	a, err := authz.Load(dir, "dev")
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

// approvedProdRun creates a change in the prod environment that is ready to
// apply: approved, with a plan artifact and a stub engine behind it.
func approvedProdRun(t *testing.T, svc *ChangeService, store state.Store) string {
	t.Helper()
	ctx := context.Background()
	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"), &pb.CreateChangeRequest{
		Label: "policy-target", Environment: "prod",
	})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())
	setRunStatus(t, store, created.GetId(), "approved")
	return created.GetId()
}

func TestApplyChange_PolicyDeniesUnregisteredAndUnauthorisedSubjects(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{runID: "exec-pol", runSuccess: true, runPhase: "completed"}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := approvedProdRun(t, svc, store)

	// Unregistered: the seat of the denial is "I do not know you".
	_, err := svc.ApplyChange(ContextWithActor(ctx, "mallory"), &pb.ApplyChangeRequest{ChangeId: id})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "not registered")

	// Registered, environment reachable, but no axis grants apply.
	_, err = svc.ApplyChange(ContextWithActor(ctx, "carol"), &pb.ApplyChangeRequest{ChangeId: id})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "neither the matrix nor role")
	assert.Contains(t, err.Error(), "levee authz explain", "the refusal must point at the diagnostic")

	assert.Equal(t, int32(0), atomic.LoadInt32(&rec.runCalled), "a denied apply must never reach the engine")

	// An enforced policy with no identifiable caller cannot be judged at all.
	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: id})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Equal(t, int32(0), atomic.LoadInt32(&rec.runCalled))
}

func TestApplyChange_PolicyAllowsViaRoleInsideReachableEnvironment(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{runID: "exec-ok", runSuccess: true, runPhase: "completed"}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := approvedProdRun(t, svc, store)

	// alice's team may only view prod; her role supplies apply.
	resp, err := svc.ApplyChange(ContextWithActor(ctx, "alice"), &pb.ApplyChangeRequest{ChangeId: id})
	require.NoError(t, err)
	assert.True(t, resp.GetSuccess())
	assert.Equal(t, int32(1), atomic.LoadInt32(&rec.runCalled))
}

func TestApproveChange_PolicyDenialPrecedesApprovalList(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeServiceWithEngine(t, (&recordingEngine{}).adapter())
	svc.WithAuthorizer(newPolicyAuthorizer(t))

	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"), &pb.CreateChangeRequest{
		Label: "approve-under-policy", Environment: "prod",
	})
	require.NoError(t, err)
	persistPlanOnRun(t, store, created.GetId())

	// carol is a legitimate sre member but her role cannot approve anywhere,
	// and prod grants the team only view.
	_, err = svc.ApproveChange(ContextWithActor(ctx, "carol"), &pb.ApproveRequest{
		ChangeId: created.GetId(), Approver: "carol",
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "approve denied")

	// The run was not settled by the refused vote.
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	assert.NotEqual(t, "approved", run.Status)
}

// TestNoAuthorizerKeepsPreviousBehaviour is the compatibility guard: the
// posture says an unconfigured deployment notices NO change, and that promise
// has to be enforced by a test rather than by good intentions.
func TestNoAuthorizerKeepsPreviousBehaviour(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{runID: "exec-free", runSuccess: true, runPhase: "completed"}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())
	// Deliberately no WithAuthorizer.
	id := approvedProdRun(t, svc, store)

	resp, err := svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: id})
	require.NoError(t, err, "without a policy the deployment behaves exactly as before")
	assert.True(t, resp.GetSuccess())
	assert.Equal(t, int32(1), atomic.LoadInt32(&rec.runCalled))
}
