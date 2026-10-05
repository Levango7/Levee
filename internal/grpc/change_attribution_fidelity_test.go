// change_attribution_fidelity_test.go pins WHO STARTED a change and in WHICH
// environment, for the two creation paths that do not go through
// CreateChange: `InstantiateTemplate` (the template library's "turn this
// template into a change" RPC) and `CloneChange`.
//
// Both paths were fixed for CreateChange in the subject-binding round
// (see subject.go): run.Creator feeds the approval chain's Initiator, and
// the environment marker feeds every later policy decision. A creation path
// that writes something else does not merely record untidy data — it moves
// the change into a different governance universe:
//
//   - Creator "grpc" (or a client-asserted actor) can never equal a real
//     subject's name, so exclude_initiator silently stops excluding anybody:
//     the author of a high-tier change approves their own change and the
//     chain settles;
//   - an empty environment marker makes `authz.Decide` fall back to
//     permission.default_env, so a change DECLARED for prod is judged with
//     the caller's dev grants for the rest of its life.
//
// These tests are the probe: they are written against the intended
// behaviour, so each one fails until the path carries the fidelity.
package grpc

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// devOnlyMatrix: team sre may act in dev and may only look at prod.
// permission.default_env for the Authorizer below is "dev", which is the
// exact shape that turns a dropped environment marker into an escalation.
//
// alice's role deliberately carries only view: the composition is "the matrix
// says WHERE, the role says WHAT inside those environments" (see package
// authz), so an operator role would grant her apply in prod through the role
// axis and the test would prove nothing about the environment marker.
const devOnlyMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
      - name: prod
        actions: [view]
`

const attributionRolesYAML = `
roles:
  - name: viewer
    permissions: [view]
`

const attributionUsersYAML = `
users:
  - name: alice
    team: sre
    role: viewer
`

func newAttributionAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(devOnlyMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.RoleTreeFileName), []byte(attributionRolesYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(attributionUsersYAML), 0o600))

	a, err := authz.Load(dir, "dev")
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

// sharedStoreServices returns a TemplateService and a ChangeService over ONE
// store, because the defect under test is what the second service reads back
// from what the first wrote. The ChangeService carries no approval service:
// these tests judge the policy gate and the stored row, not the chain.
func sharedStoreServices(t *testing.T, engine *recordingEngine) (*TemplateService, *ChangeService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	return NewTemplateService(store, nil), NewChangeService(store, engine.adapter(), nil, nil), store
}

// sharedStoreServicesWithApproval is the same pair with the REAL approval
// service wired, for the tests that walk a chain to decision time.
func sharedStoreServicesWithApproval(t *testing.T, engine *recordingEngine) (*TemplateService, *ChangeService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	change := NewChangeService(store, engine.adapter(), approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	return NewTemplateService(store, nil), change, store
}

// instantiateChangeIn creates "restart-web" as a template and instantiates it
// as a change declared for the given environment, the way a caller over the
// wire would.
func instantiateChangeIn(t *testing.T, tmpl *TemplateService, subject, env string) string {
	t.Helper()
	ctx := ContextWithSubject(context.Background(), subject)
	_, err := tmpl.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "restart-web", WorkflowContent: "name: restart\n", Overwrite: true,
	})
	require.NoError(t, err)

	created, err := tmpl.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "restart-web",
		Environment:  env,
	})
	require.NoError(t, err)
	return created.GetId()
}

func instantiateProdChange(t *testing.T, tmpl *TemplateService, subject string) string {
	t.Helper()
	return instantiateChangeIn(t, tmpl, subject, "prod")
}

// TestInstantiateTemplate_PersistsTheDeclaredEnvironment is the data-fidelity
// half: the RPC echoes Environment back to the caller, so a client believes
// it created a prod change even though the stored row says otherwise.
func TestInstantiateTemplate_PersistsTheDeclaredEnvironment(t *testing.T) {
	tmpl, _, store := sharedStoreServices(t, &recordingEngine{})
	id := instantiateProdChange(t, tmpl, "alice")

	run, err := store.GetRun(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "prod", run.IncidentID,
		"the declared environment must be the persisted marker: every later policy decision reads it via envOf()")
}

// TestInstantiateTemplate_ProdChangeIsNotJudgedWithDevGrants is the reason the
// above is a security property and not a tidy-data one. alice may apply in
// dev and may only view prod. If the declared prod is dropped, Decide() falls
// back to permission.default_env = dev and her dev grants are consulted for a
// change that targets prod.
func TestInstantiateTemplate_ProdChangeIsNotJudgedWithDevGrants(t *testing.T) {
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: planWithFloor(t, "standard"),
		runID: "exec-laundered", runSuccess: true, runPhase: "completed"}
	tmpl, change, store := sharedStoreServices(t, engine)
	change.WithAuthorizer(newAttributionAuthorizer(t))

	id := instantiateProdChange(t, tmpl, "alice")
	persistPlanOnRun(t, store, id)
	setRunStatus(t, store, id, "approved")

	_, err := change.ApplyChange(ContextWithSubject(context.Background(), "alice"),
		&pb.ApplyChangeRequest{ChangeId: id})
	require.Error(t, err, "a prod change must not be applicable on dev grants")
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
	assert.Contains(t, err.Error(), `env "prod"`, "the refusal must name the environment it judged")
	assert.Equal(t, int32(0), atomic.LoadInt32(&engine.runCalled), "a denied apply must never reach the engine")

	// The same caller, the same template, declared for dev: allowed. Without
	// this half the test could not tell "the marker is honoured" from "the
	// policy refuses everything".
	devID := instantiateChangeIn(t, tmpl, "alice", "dev")
	persistPlanOnRun(t, store, devID)
	setRunStatus(t, store, devID, "approved")
	resp, err := change.ApplyChange(ContextWithSubject(context.Background(), "alice"),
		&pb.ApplyChangeRequest{ChangeId: devID})
	require.NoError(t, err, "%v", err)
	assert.True(t, resp.GetSuccess())
}

// TestInstantiateTemplate_RecordsVerifiedSubjectAsCreator pins attribution:
// "grpc" is a channel, not a person, and the approval chain's independence
// rule compares names.
func TestInstantiateTemplate_RecordsVerifiedSubjectAsCreator(t *testing.T) {
	tmpl, _, store := sharedStoreServices(t, &recordingEngine{})
	id := instantiateProdChange(t, tmpl, "alice")

	run, err := store.GetRun(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "alice", run.Creator,
		"run.Creator becomes the approval chain's Initiator")
}

// TestInstantiateTemplate_ResponseAgreesWithTheStoredRow is what made the two
// defects invisible from the client side: the RPC echoed the declared
// environment back while persisting nothing, so a caller had no signal that
// its change was heading into a different governance universe.
func TestInstantiateTemplate_ResponseAgreesWithTheStoredRow(t *testing.T) {
	ctx := ContextWithSubject(context.Background(), "alice")
	tmpl, _, store := sharedStoreServices(t, &recordingEngine{})
	_, err := tmpl.CreateTemplate(ctx, &pb.CreateTemplateRequest{
		Name: "restart-web", WorkflowContent: "name: restart\n",
	})
	require.NoError(t, err)

	created, err := tmpl.InstantiateTemplate(ctx, &pb.InstantiateTemplateRequest{
		TemplateName: "restart-web", Environment: "prod",
	})
	require.NoError(t, err)

	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, created.GetEnvironment(), run.IncidentID, "the echoed environment is a promise about the row")
	assert.Equal(t, created.GetCreatedBy(), run.Creator, "the echoed owner is a promise about the row")
}

// TestInstantiateTemplate_RecordsCreationInTheAuditTrail covers the third
// thing CreateChange does and this path did not: a change that appears in the
// store without an audit row is missing from the tamper-evident trail that
// `VerifyHashChain` and `levee audit log` are read to answer.
func TestInstantiateTemplate_RecordsCreationInTheAuditTrail(t *testing.T) {
	tmpl, _, store := sharedStoreServices(t, &recordingEngine{})
	id := instantiateProdChange(t, tmpl, "alice")

	entries, err := store.ListAudits(context.Background(), state.AuditFilter{RunID: id})
	require.NoError(t, err)
	require.Len(t, entries, 1, "template instantiation must leave exactly one creation row")
	assert.Equal(t, "create", entries[0].Action)
	assert.Equal(t, "alice", entries[0].Actor, "the audit row names the same identity the run is owned by")
	assert.Equal(t, "restart-web", entries[0].Target)
}

// TestInstantiateTemplate_AuthorCannotSelfApproveWhenIndependenceRequired is
// the governance consequence of the row above, walked end to end through the
// real approval service: plan the change (which starts the chain with the
// recorded initiator), then let that same caller vote.
func TestInstantiateTemplate_AuthorCannotSelfApproveWhenIndependenceRequired(t *testing.T) {
	sp := planWithGovernance(t, "standard", &dsl.ApprovalSpec{
		Approvers:        []string{"alice", "bob"},
		MinApprovers:     1,
		ExcludeInitiator: true,
	})
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	tmpl, change, _ := sharedStoreServicesWithApproval(t, engine)
	ctx := ContextWithSubject(context.Background(), "alice")

	id := instantiateProdChange(t, tmpl, "alice")
	_, err := change.PlanChange(ctx, &pb.PlanChangeRequest{ChangeId: id, TargetHosts: []string{"web-1"}})
	require.NoError(t, err)

	_, err = change.ApproveChange(ctx, &pb.ApproveRequest{ChangeId: id, Approver: "alice"})
	require.Error(t, err, "the author of a template-created change is still its author")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "independent review")
}

// TestCloneChange_RecordsVerifiedSubjectNotTheAssertedActor covers the third
// creation path. The asserted actor is whatever the client typed into
// x-actor; with a verified subject present it must lose. Otherwise a caller
// can clone under somebody else's name and the independence rule then excludes
// the wrong person — while the real author votes freely.
func TestCloneChange_RecordsVerifiedSubjectNotTheAssertedActor(t *testing.T) {
	_, change, store := sharedStoreServices(t, &recordingEngine{})
	ctx := ContextWithSubject(context.Background(), "alice")

	source, err := change.CreateChange(ctx, &pb.CreateChangeRequest{Label: "source"})
	require.NoError(t, err)

	// The auth layer has already fixed the subject; the logging interceptor
	// then overwrites the audit LABEL from the client's x-actor header.
	asserted := context.WithValue(ctx, actorKey{}, "mallory")
	cloned, err := change.CloneChange(asserted, &pb.CloneChangeRequest{SourceChangeId: source.GetId()})
	require.NoError(t, err)

	run, err := store.GetRun(context.Background(), cloned.GetId())
	require.NoError(t, err)
	assert.Equal(t, "alice", run.Creator, "the verified subject owns the change, not the name on the header")
	assert.NotEqual(t, "mallory", run.Creator)
}

// TestCloneChange_KeepsTheAssertedLabelWithoutASubject is the compatibility
// half of the same rule: with no verifiable identity there is nothing to
// prefer, and inventing "grpc" there would erase the only record of who ran
// the command (this is the posture CreateChange already documents).
func TestCloneChange_KeepsTheAssertedLabelWithoutASubject(t *testing.T) {
	_, change, store := sharedStoreServices(t, &recordingEngine{})
	asserted := context.WithValue(context.Background(), actorKey{}, "sre-oncall")

	source, err := change.CreateChange(asserted, &pb.CreateChangeRequest{Label: "source2"})
	require.NoError(t, err)
	cloned, err := change.CloneChange(asserted, &pb.CloneChangeRequest{SourceChangeId: source.GetId()})
	require.NoError(t, err)

	run, err := store.GetRun(context.Background(), cloned.GetId())
	require.NoError(t, err)
	assert.Equal(t, "sre-oncall", run.Creator)
}
