// serve_policy_wiring_test.go proves the property the service-level tests
// cannot: that the process `levee serve` actually starts installs the policy
// authorizer into every service it registers.
//
// A gate that exists in a service struct is not a gate. Before this file, no
// test in cmd/levee referenced the authorizer at all — `changeSvc.WithAuthorizer`
// was a line in one function, and the fleet services were constructed in two
// different places (buildServeServices and the registration site), so nothing
// failed if either stopped passing it. What is asserted here is the observable
// consequence of the wiring: a subject the matrix does not grant is refused by
// the service the running process would use, and an unconfigured deployment is
// not.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// The matrix grants the sre team everything EXCEPT admin in dev, and platform
// admin — so "denied" here can only come from the policy being consulted, and
// "allowed for dave but not alice" can only come from the matrix itself.
const wiringMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
  - name: platform
    environments:
      - name: dev
        actions: [admin]
`

const wiringRolesYAML = `
roles:
  - name: operator
    permissions: [view]
  - name: steward
    permissions: [view]
`

const wiringUsersYAML = `
users:
  - name: alice
    team: sre
    role: operator
  - name: dave
    team: platform
    role: steward
`

// newWiringServices builds the services exactly the way `levee serve` does,
// over a throwaway data directory and store.
func newWiringServices(t *testing.T, withMatrix bool) (serveServices, state.Store) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o755))
	if withMatrix {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "permissions.yaml"), []byte(wiringMatrixYAML), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "roles.yaml"), []byte(wiringRolesYAML), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, "users.yaml"), []byte(wiringUsersYAML), 0o600))
	}

	store, err := state.NewSQLiteStore(context.Background(), filepath.Join(dir, "levee.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	cfg := &config.Config{}
	cfg.Server.DataDir = dataDir
	cfg.Permission.DefaultEnv = "dev"

	svcs, err := buildServeServices(store, cfg, nil)
	require.NoError(t, err, "buildServeServices must work on a zero config")
	return svcs, store
}

func deniedAs(t *testing.T, want codes.Code, err error, rpc string) {
	t.Helper()
	require.Error(t, err, "%s reached the service without judgement", rpc)
	st, ok := status.FromError(err)
	require.True(t, ok, "%s: %v", rpc, err)
	assert.Equal(t, want, st.Code(), "%s: %v", rpc, err)
}

// TestServeInstallsPolicyInEveryService is the wiring guard: each of the six
// services the process registers refuses an unknown subject, through the code
// path a real request would take.
func TestServeInstallsPolicyInEveryService(t *testing.T) {
	svcs, store := newWiringServices(t, true)
	require.True(t, svcs.authzSvc.Enforced(), "the fixture must produce an enforced matrix")

	ctx := grpc.ContextWithSubject(context.Background(), "mallory")

	// A prod run, so the audit and change paths have something real to judge.
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: "run-prod", WorkflowName: "name: prod\n", Status: "approved", IncidentID: "prod",
	}))

	// Every refusal below is PermissionDenied rather than Unauthenticated
	// because the subject IS named — it is simply not in users.yaml. "I do not
	// know you" lands on the refusing side of an enforced matrix, and an
	// anonymous caller (no subject at all) is the Unauthenticated case, pinned
	// in internal/grpc/service_policy_test.go.
	_, err := svcs.targetSvc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	deniedAs(t, codes.PermissionDenied, err, "TargetService.AddTarget")
	_, err = svcs.inventorySvc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "g"})
	deniedAs(t, codes.PermissionDenied, err, "InventoryService.CreateGroup")
	_, err = svcs.templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{Name: "t", WorkflowContent: "x"})
	deniedAs(t, codes.PermissionDenied, err, "TemplateService.CreateTemplate")
	_, err = svcs.systemSvc.GetConfig(ctx, &pb.GetConfigRequest{})
	deniedAs(t, codes.PermissionDenied, err, "SystemService.GetConfig")
	_, err = svcs.auditSvc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-prod"})
	deniedAs(t, codes.PermissionDenied, err, "AuditService.GetRunReport")
	_, err = svcs.changeSvc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: "run-prod"})
	deniedAs(t, codes.PermissionDenied, err, "ChangeService.ApplyChange")
}

// TestServePolicyDeniesFleetWritesThatChangeGrantsAllow is the half that makes
// the previous test more than "unknown subject is refused": alice is a real,
// registered team member whose matrix entry covers every change action, yet
// the fleet writes still stop at her — because `admin` is what they ask for.
func TestServePolicyDeniesFleetWritesThatChangeGrantsAllow(t *testing.T) {
	svcs, _ := newWiringServices(t, true)
	alice := grpc.ContextWithSubject(context.Background(), "alice")

	_, err := svcs.changeSvc.CreateChange(alice, &pb.CreateChangeRequest{Label: "ok", Environment: "dev"})
	require.NoError(t, err, "alice may create a change in dev: %v", err)

	_, err = svcs.targetSvc.AddTarget(alice, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	deniedAs(t, codes.PermissionDenied, err, "AddTarget (no admin grant)")
	_, err = svcs.templateSvc.CreateTemplate(alice, &pb.CreateTemplateRequest{Name: "t", WorkflowContent: "x"})
	deniedAs(t, codes.PermissionDenied, err, "CreateTemplate (no admin grant)")
	_, err = svcs.systemSvc.GetConfig(alice, &pb.GetConfigRequest{})
	deniedAs(t, codes.PermissionDenied, err, "GetConfig (no admin grant)")

	// dave's team holds admin, so the same calls work for him. The refusal
	// above is the matrix speaking, not a hard-coded deny.
	_, err = svcs.targetSvc.AddTarget(grpc.ContextWithSubject(context.Background(), "dave"),
		&pb.AddTargetRequest{Id: "t2", Hostname: "h2"})
	require.NoError(t, err, "dave has admin in dev: %v", err)
}

// TestServeWithoutMatrixKeepsPreviousBehaviour is the compatibility promise at
// the wiring level: an operator who never wrote permissions.yaml gets the
// binary they had yesterday, for every one of these RPCs.
func TestServeWithoutMatrixKeepsPreviousBehaviour(t *testing.T) {
	svcs, store := newWiringServices(t, false)
	require.False(t, svcs.authzSvc.Enforced())

	ctx := grpc.ContextWithSubject(context.Background(), "anyone")
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID: "run-1", WorkflowName: "name: x\n", Status: "approved",
	}))

	_, err := svcs.targetSvc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	require.NoError(t, err, "%v", err)
	_, err = svcs.inventorySvc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "g"})
	require.NoError(t, err, "%v", err)
	_, err = svcs.templateSvc.CreateTemplate(ctx, &pb.CreateTemplateRequest{Name: "t", WorkflowContent: "x"})
	require.NoError(t, err, "%v", err)
	_, err = svcs.systemSvc.GetConfig(ctx, &pb.GetConfigRequest{})
	require.NoError(t, err, "%v", err)
	_, err = svcs.auditSvc.GetRunReport(ctx, &pb.GetRunReportRequest{RunId: "run-1"})
	require.NoError(t, err, "%v", err)
}
