// service_policy_test.go pins what the permission matrix has to say about the
// five services that are not change-scoped: target inventory, asset
// management, the template library, the audit trail and the system surface.
//
// Until now those RPCs shared one property: they were authenticated and
// otherwise unpoliced. `grep -c authz` over the five service files returned
// zero, and the consequence is not "nobody can do anything" but the opposite —
// a deployment that wrote `permissions.yaml` to keep the dev team out of prod
// enforced that only on the change path. The same caller could enumerate every
// prod hostname and its credential reference (`ListTargets`), freeze a prod
// host (`SetTargetStatus`), delete the template another team's change is built
// from, read the whole audit trail, or dump the redacted config.
//
// The rule these tests encode, and the reason each piece is where it is:
//
//   - scope comes from the resource's own stated environment. A target's is
//     `labels["env"]`; a change's is the run's environment column. A group, a
//     template or a config document declares none, so they are judged in
//     permission.default_env — and with no default configured the answer is a
//     refusal, not a guess (authz.Decide: "an authorisation question with no
//     environment cannot be answered against an environment-keyed policy").
//   - fleet-wide WRITES need `admin`. Registering, removing, freezing or
//     bulk-importing hosts decides what LEVEE is allowed to reach; that is not
//     something a team that may `apply` in dev should get by accident.
//   - READS use `view`, and a list NARROWS to what the caller may see instead
//     of refusing the page — refusing would also hide the rows they are
//     entitled to (same reasoning as ListChanges).
//   - an unattributable caller (shared token) keeps passing reads and still
//     cannot write, which is the existing documented posture, not a new one.
package grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// The fixture is deliberately shaped so that every posture clause has a
// subject that satisfies it and one that does not:
//
//	alice  sre/viewer      → view+plan in dev, view in staging, admin nowhere
//	dave   platform/viewer → admin in dev, nothing in staging or prod
//	carol  auditors/clerk  → approve in dev only, so not even `view`
//	mallory                → not registered at all
const serviceMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, view]
      - name: staging
        actions: [view]
  - name: platform
    environments:
      - name: dev
        actions: [admin]
  - name: auditors
    environments:
      - name: dev
        actions: [approve]
`

const serviceRolesYAML = `
roles:
  - name: viewer
    permissions: [view]
  - name: clerk
    permissions: [approve]
`

const serviceUsersYAML = `
users:
  - name: alice
    team: sre
    role: viewer
  - name: dave
    team: platform
    role: viewer
  - name: carol
    team: auditors
    role: clerk
`

// serviceDefaultEnv is the environment every unscoped resource (groups,
// templates, config) is judged in.
const serviceDefaultEnv = "dev"

func newServiceAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(serviceMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.RoleTreeFileName), []byte(serviceRolesYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(serviceUsersYAML), 0o600))

	a, err := authz.Load(dir, serviceDefaultEnv)
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

func asSubject(subject string) context.Context {
	return ContextWithSubject(context.Background(), subject)
}

// codeOf keeps a failure message readable: the code alone ("expected 0x7") is
// the least informative part of a refused RPC.
func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "refusal must be a gRPC status, got %v", err)
	return st.Code()
}

func requireDenied(t *testing.T, err error, rpc string) {
	t.Helper()
	require.Error(t, err, "%s must not be wide open", rpc)
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err), "%s: %v", rpc, err)
}

// --- seeding -----------------------------------------------------------------

func seedTarget(t *testing.T, store state.Store, id, host, env string) {
	t.Helper()
	labels := map[string]string{}
	if env != "" {
		labels["env"] = env
	}
	require.NoError(t, store.UpsertTarget(context.Background(), &state.Target{
		ID:          id,
		Hostname:    host,
		Port:        22,
		ChannelType: "ssh",
		Labels:      labels,
		Status:      state.StatusActive,
	}))
}

func seedRunInEnv(t *testing.T, store state.Store, id, env, creator string) {
	t.Helper()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID:           id,
		WorkflowName: "name: seeded\n",
		Status:       "completed",
		Creator:      creator,
		IncidentID:   env,
	}))
}

// --- target inventory --------------------------------------------------------

func TestServicePolicy_TargetWritesRequireAdmin(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithAuthorizer(newServiceAuthorizer(t))

	// alice may plan and view dev; registering a host is not that.
	_, err := svc.AddTarget(asSubject("alice"), &pb.AddTargetRequest{
		Id: "t1", Hostname: "dev-1", Labels: map[string]string{"env": "dev"},
	})
	requireDenied(t, err, "AddTarget")
	assert.Contains(t, err.Error(), `"admin"`, "the refusal must name the action it judged")

	// Unlabelled requests are judged in the default environment, not exempted
	// from judgement: "no environment" is not a scope nobody can have.
	_, err = svc.AddTarget(asSubject("alice"), &pb.AddTargetRequest{Id: "t2", Hostname: "dev-2"})
	requireDenied(t, err, "AddTarget (unlabelled)")

	// dave has admin in dev, so dev registrations work for him…
	_, err = svc.AddTarget(asSubject("dave"), &pb.AddTargetRequest{
		Id: "t3", Hostname: "dev-3", Labels: map[string]string{"env": "dev"},
	})
	require.NoError(t, err, "%v", err)

	// …and platform may not reach staging at all.
	_, err = svc.AddTarget(asSubject("dave"), &pb.AddTargetRequest{
		Id: "t4", Hostname: "stg-4", Labels: map[string]string{"env": "staging"},
	})
	requireDenied(t, err, "AddTarget (staging)")
	assert.Contains(t, err.Error(), "staging")

	// Removal is judged against the STORED row's environment, which the caller
	// cannot rewrite — unlike the label on an add request.
	seedTarget(t, store, "prod-1", "prod-1.example", "prod")
	_, err = svc.RemoveTarget(asSubject("dave"), &pb.RemoveTargetRequest{Id: "prod-1"})
	requireDenied(t, err, "RemoveTarget")

	// No verifiable identity, no write (this half predates the matrix).
	_, err = svc.AddTarget(context.Background(), &pb.AddTargetRequest{Id: "t5", Hostname: "dev-5"})
	assert.Equal(t, codes.Unauthenticated, codeOf(t, err), "an unattributable write must be Unauthenticated")
}

func TestServicePolicy_TargetReadsUseViewScope(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithAuthorizer(newServiceAuthorizer(t))
	seedTarget(t, store, "dev-1", "dev-1.example", "dev")
	seedTarget(t, store, "prod-1", "prod-1.example", "prod")

	_, err := svc.GetTarget(asSubject("alice"), &pb.GetTargetRequest{Id: "dev-1"})
	require.NoError(t, err, "alice views dev: %v", err)

	_, err = svc.GetTarget(asSubject("alice"), &pb.GetTargetRequest{Id: "prod-1"})
	requireDenied(t, err, "GetTarget (prod)")

	// carol's team is granted only approve in dev: she may not even look.
	_, err = svc.GetTarget(asSubject("carol"), &pb.GetTargetRequest{Id: "dev-1"})
	requireDenied(t, err, "GetTarget (no view grant)")

	// An unknown subject lands on the refusing side ("I do not know you").
	_, err = svc.GetTarget(asSubject("mallory"), &pb.GetTargetRequest{Id: "dev-1"})
	requireDenied(t, err, "GetTarget (unregistered)")
	assert.Contains(t, err.Error(), "not registered")

	// A host without an env label is judged in the default environment, so it
	// is visible to exactly the callers who may see dev.
	seedTarget(t, store, "bare-1", "bare-1.example", "")
	_, err = svc.GetTarget(asSubject("alice"), &pb.GetTargetRequest{Id: "bare-1"})
	require.NoError(t, err, "unlabelled target falls to the default env alice can view: %v", err)
	_, err = svc.GetTarget(asSubject("carol"), &pb.GetTargetRequest{Id: "bare-1"})
	requireDenied(t, err, "GetTarget (unlabelled, no view)")
}

// TestServicePolicy_ListTargetsNarrowsInsteadOfRefusing is the list half of the
// read rule: alice may see dev, so she gets dev — not an error that would also
// hide the row she is entitled to.
func TestServicePolicy_ListTargetsNarrowsInsteadOfRefusing(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithAuthorizer(newServiceAuthorizer(t))
	seedTarget(t, store, "dev-1", "dev-1.example", "dev")
	seedTarget(t, store, "stg-1", "stg-1.example", "staging")
	seedTarget(t, store, "prod-1", "prod-1.example", "prod")

	resp, err := svc.ListTargets(asSubject("alice"), &pb.ListTargetsRequest{})
	require.NoError(t, err, "%v", err)
	visible := map[string]bool{}
	for _, row := range resp.GetTargets() {
		visible[row.GetId()] = true
	}
	assert.True(t, visible["dev-1"], "dev is visible to alice: %+v", visible)
	assert.True(t, visible["stg-1"], "staging is viewable by alice's team: %+v", visible)
	assert.False(t, visible["prod-1"], "prod must not be enumerable: %+v", visible)
	assert.Equal(t, int32(2), resp.GetTotalSize(),
		"the total describes what this caller may see, or a pager shows rows it cannot fetch")

	// The documented exemption: with no provable identity every holder of the
	// shared token is the same principal, so filtering them hides nothing and
	// only darkens the dashboard.
	unattributed, err := svc.ListTargets(context.Background(), &pb.ListTargetsRequest{})
	require.NoError(t, err, "%v", err)
	assert.Len(t, unattributed.GetTargets(), 3, "an unattributable caller is not filtered")
}

func TestServicePolicy_CheckTargetIsAReadOnTheTargetsEnvironment(t *testing.T) {
	store := newTestStore(t)
	svc := NewTargetService(store, nil).WithAuthorizer(newServiceAuthorizer(t))
	seedTarget(t, store, "dev-1", "dev-1.example", "dev")
	seedTarget(t, store, "prod-1", "prod-1.example", "prod")

	// A probe opens a connection to the host and stamps a derived field, but it
	// is a diagnostic: view is the right scope, and prod stays out of reach.
	_, err := svc.CheckTarget(asSubject("alice"), &pb.CheckTargetRequest{Id: "prod-1"})
	requireDenied(t, err, "CheckTarget (prod)")

	_, err = svc.CheckTarget(asSubject("alice"), &pb.CheckTargetRequest{Id: "dev-1"})
	require.NoError(t, err, "%v", err)
}

// --- asset management (InventoryService) -------------------------------------

func TestServicePolicy_InventoryWritesRequireAdmin(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithAuthorizer(newServiceAuthorizer(t))

	_, err := svc.CreateGroup(asSubject("alice"), &pb.CreateGroupRequest{Name: "dev/web"})
	requireDenied(t, err, "CreateGroup")

	_, err = svc.CreateGroup(asSubject("dave"), &pb.CreateGroupRequest{Name: "dev/web"})
	require.NoError(t, err, "admin in the default env may create groups: %v", err)

	seedTarget(t, store, "prod-1", "prod-1.example", "prod")
	_, err = svc.SetTargetStatus(asSubject("dave"), &pb.SetTargetStatusRequest{TargetId: "prod-1", Status: state.StatusFrozen})
	requireDenied(t, err, "SetTargetStatus")
	stored, err := store.GetTarget(context.Background(), "prod-1")
	require.NoError(t, err)
	assert.Equal(t, state.StatusActive, stored.Status,
		"a refused lifecycle write must not touch the row — freezing a prod host is an outage")

	_, err = svc.SetTargetStatus(asSubject("dave"), &pb.SetTargetStatusRequest{TargetId: "prod-1", Status: state.StatusActive})
	requireDenied(t, err, "SetTargetStatus (even back to active)")

	_, err = svc.DeleteGroup(asSubject("alice"), &pb.DeleteGroupRequest{Id: "missing"})
	requireDenied(t, err, "DeleteGroup")

	_, err = svc.ListGroups(asSubject("alice"), &pb.ListGroupsRequest{})
	require.NoError(t, err, "groups are namespaced in the default env alice can view: %v", err)

	_, err = svc.ListGroups(asSubject("carol"), &pb.ListGroupsRequest{})
	requireDenied(t, err, "ListGroups (no view grant)")
}

// TestServicePolicy_ImportTargetsIsJudgedPerDeclaredEnvironment closes the gap
// between the bulk and the single-host path: an inventory file is a pile of
// AddTarget requests, so it must not be authorisable in a weaker scope than
// one of them is.
func TestServicePolicy_ImportTargetsIsJudgedPerDeclaredEnvironment(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithAuthorizer(newServiceAuthorizer(t))

	_, err := svc.ImportTargets(asSubject("dave"), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: dev-9\n",
	})
	require.NoError(t, err, "a dev-only import for the dev admin: %v", err)

	_, err = svc.ImportTargets(asSubject("dave"), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: dev-9\n  - address: prod-9\n    labels:\n      env: prod\n",
	})
	requireDenied(t, err, "ImportTargets (one prod entry)")
	assert.Contains(t, err.Error(), "prod", "the refusal must name the environment it cannot authorise")

	_, err = svc.ImportTargets(asSubject("alice"), &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: dev-10\n",
	})
	requireDenied(t, err, "ImportTargets")

	// Nothing from the refused file reached the store.
	_, err = store.GetTarget(context.Background(), "prod-9")
	require.NoError(t, err)
}

func TestServicePolicy_TargetHistoryShowsOnlyVisibleRuns(t *testing.T) {
	store := newTestStore(t)
	svc := NewInventoryService(store).WithAuthorizer(newServiceAuthorizer(t))
	seedRunInEnv(t, store, "run-dev", "dev", "alice")
	seedRunInEnv(t, store, "run-prod", "prod", "alice")
	// Steps carry a foreign key to their batch, so the history rows need the
	// whole chain: run → batch → step on the host under query.
	for _, id := range []string{"run-dev", "run-prod"} {
		require.NoError(t, store.CreateBatch(context.Background(), &state.Batch{
			ID: "bat-" + id, RunID: id, BatchNo: 1, Status: "completed", TotalHosts: 1, Succeeded: 1,
		}))
		require.NoError(t, store.CreateStep(context.Background(), &state.Step{
			ID: "step-" + id, RunID: id, BatchID: "bat-" + id, Host: "web-1", Status: "success",
		}))
	}

	resp, err := svc.TargetHistory(asSubject("alice"), &pb.TargetHistoryRequest{Host: "web-1"})
	require.NoError(t, err, "%v", err)
	got := map[string]bool{}
	for _, e := range resp.GetEntries() {
		got[e.GetRunId()] = true
	}
	assert.True(t, got["run-dev"])
	assert.False(t, got["run-prod"],
		"TargetHistory hands out the workflow document and creator of every run that touched a host")
}

// --- template library --------------------------------------------------------

func TestServicePolicy_TemplateWritesRequireAdmin(t *testing.T) {
	store := newTestStore(t)
	svc := NewTemplateService(store, nil).WithAuthorizer(newServiceAuthorizer(t))
	ctx := asSubject("alice")

	_, err := svc.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "restart", WorkflowContent: "name: restart\n",
	})
	requireDenied(t, err, "CreateTemplate")

	_, err = svc.CreateTemplate(asSubject("dave"), &pb.CreateTemplateRequest{
		Name: "restart", WorkflowContent: "name: restart\n",
	})
	require.NoError(t, err, "%v", err)

	_, err = svc.DeleteTemplate(ctx, &pb.DeleteTemplateRequest{Name: "restart"})
	requireDenied(t, err, "DeleteTemplate")

	_, err = svc.GetTemplate(ctx, &pb.GetTemplateRequest{Name: "restart"})
	require.NoError(t, err, "reading a template is a view: %v", err)

	_, err = svc.DeleteTemplate(asSubject("dave"), &pb.DeleteTemplateRequest{Name: "restart"})
	require.NoError(t, err, "%v", err)
}

func TestServicePolicy_InstantiateIsAPlanInTheDeclaredEnvironment(t *testing.T) {
	store := newTestStore(t)
	svc := NewTemplateService(store, nil).WithAuthorizer(newServiceAuthorizer(t))
	_, err := svc.CreateTemplate(asSubject("dave"), &pb.CreateTemplateRequest{
		Name: "restart", WorkflowContent: "name: restart\n",
	})
	require.NoError(t, err, "%v", err)

	// Instantiating creates a change, so it is the plan action — and `plan` is
	// a write: it persists an artifact and can reset somebody's approval.
	_, err = svc.InstantiateTemplate(asSubject("carol"), &pb.InstantiateTemplateRequest{
		TemplateName: "restart", Environment: "dev",
	})
	requireDenied(t, err, "InstantiateTemplate (no plan grant)")

	_, err = svc.InstantiateTemplate(asSubject("alice"), &pb.InstantiateTemplateRequest{
		TemplateName: "restart", Environment: "prod",
	})
	requireDenied(t, err, "InstantiateTemplate (prod)")

	_, err = svc.InstantiateTemplate(asSubject("alice"), &pb.InstantiateTemplateRequest{
		TemplateName: "restart", Environment: "dev",
	})
	require.NoError(t, err, "alice plans in dev: %v", err)
}

// --- audit trail -------------------------------------------------------------

func TestServicePolicy_AuditReadsAreScopedPerRun(t *testing.T) {
	store := newTestStore(t)
	svc := NewAuditService(store).WithAuthorizer(newServiceAuthorizer(t))
	seedRunInEnv(t, store, "run-dev", "dev", "alice")
	seedRunInEnv(t, store, "run-prod", "prod", "alice")
	for _, id := range []string{"run-dev", "run-prod"} {
		require.NoError(t, store.CreateAudit(context.Background(), &state.Audit{
			ID: "aud-" + id, RunID: id, Action: "apply", Actor: "alice", Target: "web-1",
		}))
		// One trace row per run, so VerifyHashChain reports the run instead of
		// skipping it for having nothing to check.
		require.NoError(t, store.CreateTrace(context.Background(), &state.Trace{
			ID: "trc-" + id, RunID: id, Event: "apply",
		}))
	}

	log, err := svc.GetAuditLog(asSubject("alice"), &pb.GetAuditLogRequest{})
	require.NoError(t, err, "%v", err)
	require.Len(t, log.GetEntries(), 1, "only the dev run's row is visible: %+v", log.GetEntries())
	assert.Equal(t, int32(1), log.GetTotalSize(),
		"the total must match the narrowed page or the pager believes there is more to fetch")

	// A request that names the run is a direct question about it.
	_, err = svc.GetAuditLog(asSubject("alice"), &pb.GetAuditLogRequest{RunId: "run-prod"})
	requireDenied(t, err, "GetAuditLog (prod run named)")
	_, err = svc.GetRunReport(asSubject("alice"), &pb.GetRunReportRequest{RunId: "run-prod"})
	requireDenied(t, err, "GetRunReport (prod)")
	_, err = svc.GetRunReport(asSubject("alice"), &pb.GetRunReportRequest{RunId: "run-dev"})
	require.NoError(t, err, "%v", err)

	traces, err := svc.ListAuditTraces(asSubject("alice"), &pb.ListAuditTracesRequest{})
	require.NoError(t, err, "%v", err)
	require.Len(t, traces.GetEntries(), 1, "the prod run's trace must not be listed: %+v", traces.GetEntries())
	assert.Equal(t, "trc-run-dev", traces.GetEntries()[0].GetId())

	_, err = svc.ListAuditTraces(asSubject("alice"), &pb.ListAuditTracesRequest{RunIds: []string{"run-prod"}})
	requireDenied(t, err, "ListAuditTraces (prod run named)")

	// Integrity checking is scoped too: alice verifies the runs she may see,
	// and naming a run she may not is a refusal rather than a silent skip.
	all, err := svc.VerifyHashChain(asSubject("alice"), &pb.VerifyHashChainRequest{})
	require.NoError(t, err, "%v", err)
	var checked []string
	for _, r := range all.GetRuns() {
		checked = append(checked, r.GetRunId())
	}
	assert.Equal(t, []string{"run-dev"}, checked, "VerifyHashChain walked runs outside alice's view")

	_, err = svc.VerifyHashChain(asSubject("alice"), &pb.VerifyHashChainRequest{RunId: "run-prod"})
	requireDenied(t, err, "VerifyHashChain (prod run named)")
}

// --- system surface ----------------------------------------------------------

func TestServicePolicy_SystemSurface(t *testing.T) {
	store := newTestStore(t)
	svc := NewSystemService(store, &config.Config{}, "/tmp/levee-test.yaml", "1.2.3", "abc", "today", "go1.26", time.Now()).
		WithAuthorizer(newServiceAuthorizer(t))
	seedRunInEnv(t, store, "run-dev", "dev", "alice")
	seedRunInEnv(t, store, "run-prod", "prod", "alice")
	setRunStatus(t, store, "run-dev", "running")
	setRunStatus(t, store, "run-prod", "running")

	// Version is a handshake, not a per-environment secret; the deployment
	// refuses to start unauthenticated, so this stays open on purpose.
	ver, err := svc.GetVersion(asSubject("carol"), nil)
	require.NoError(t, err, "%v", err)
	assert.Equal(t, "1.2.3", ver.GetVersion())

	// Config content is deployment-wide and carries credential references.
	_, err = svc.GetConfig(asSubject("alice"), &pb.GetConfigRequest{})
	assert.Equal(t, codes.PermissionDenied, codeOf(t, err), "GetConfig must not be a view-level read")

	// Status is a monitoring endpoint: it stays callable, but the counts it
	// derives from other teams' changes are narrowed to what the caller sees.
	st, err := svc.GetStatus(asSubject("alice"), nil)
	require.NoError(t, err, "%v", err)
	assert.Equal(t, int32(1), st.GetActiveRuns(),
		"alice sees one running change, not the deployment's total")

	unattributed, err := svc.GetStatus(context.Background(), nil)
	require.NoError(t, err, "%v", err)
	assert.Equal(t, int32(2), unattributed.GetActiveRuns(),
		"the exemption that admits unattributable reads leaves the total alone")
}

// --- compatibility -----------------------------------------------------------

// TestServicePolicy_NoAuthorizerKeepsPreviousBehaviour is the promise the
// whole posture rests on: a deployment that never wrote permissions.yaml
// notices nothing.
func TestServicePolicy_NoAuthorizerKeepsPreviousBehaviour(t *testing.T) {
	store := newTestStore(t)
	seedTarget(t, store, "prod-1", "prod-1.example", "prod")
	seedRunInEnv(t, store, "run-prod", "prod", "alice")

	targets, err := NewTargetService(store, nil).ListTargets(asSubject("mallory"), &pb.ListTargetsRequest{})
	require.NoError(t, err, "%v", err)
	assert.Len(t, targets.GetTargets(), 1)

	_, err = NewTargetService(store, nil).AddTarget(asSubject("mallory"), &pb.AddTargetRequest{Id: "t9", Hostname: "t9"})
	require.NoError(t, err, "%v", err)

	_, err = NewInventoryService(store).CreateGroup(asSubject("mallory"), &pb.CreateGroupRequest{Name: "g"})
	require.NoError(t, err, "%v", err)

	_, err = NewTemplateService(store, nil).CreateTemplate(asSubject("mallory"), &pb.CreateTemplateRequest{
		Name: "t", WorkflowContent: "x",
	})
	require.NoError(t, err, "%v", err)

	_, err = NewAuditService(store).GetAuditLog(asSubject("mallory"), &pb.GetAuditLogRequest{})
	require.NoError(t, err, "%v", err)

	cfgSvc := NewSystemService(store, &config.Config{}, "/tmp/levee-test.yaml", "1", "2", "3", "4", time.Now())
	_, err = cfgSvc.GetConfig(asSubject("mallory"), &pb.GetConfigRequest{})
	require.NoError(t, err, "%v", err)
	_, err = cfgSvc.GetStatus(asSubject("mallory"), nil)
	require.NoError(t, err, "%v", err)
}
