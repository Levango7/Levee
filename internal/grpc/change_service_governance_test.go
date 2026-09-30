// change_service_governance_test.go pins the governance contract that the
// P0-3 review found missing. Each case corresponds to an attack that
// worked before the fix, so each one is a regression test rather than a
// description of current behaviour:
//
//  1. a high-tier change whose workflow names no approvers cannot be
//     planned at all (it used to get a 1-vote chain owned by its author);
//  2. the change's initiator may not supply a vote when the tier or the
//     workflow requires independence;
//  3. a vote is attributed to the authenticated subject, never to the
//     name in the request body;
//  4. a caller that cannot be identified may not approve at all — over
//     the real interceptor chain, with a credential that authenticates
//     the deployment but not a person;
//  5. a deployment with an engine but no approval service still records
//     the derived tier, so an irreversible plan cannot be auto-approved
//     (the priority/tier column sharing made this invisible before);
//  6. over REST, a body naming a different approver than the bearer
//     token's subject is refused.
package grpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/dsl"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPlanChange_RefusesHighTierWithoutIndependentApprovers(t *testing.T) {
	ctx := context.Background()
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: planWithFloor(t, "high")}
	svc, store := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "high-no-approvers"})
	require.NoError(t, err)

	_, err = svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.Error(t, err, "a high-tier chain must not be satisfied by its author alone")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "under-provisioned")

	// Refusing must not leave a half-built chain behind that someone can
	// later mistake for a valid approval.
	rows, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: created.GetId()})
	require.NoError(t, err)
	assert.Empty(t, rows, "no approval row may exist for a refused plan")
}

func TestApproveChange_InitiatorMayNotVoteWhenIndependenceRequired(t *testing.T) {
	ctx := context.Background()
	// The workflow itself demands independence. At one required vote the
	// chain stays satisfiable without the initiator, so planning succeeds
	// and the rule is exercised where it is actually enforced: at
	// decision time. (A high-tier chain that could only be satisfied BY
	// the initiator is refused earlier — see
	// TestPlanChange_RefusesUnsatisfiableIndependentQuorum.)
	sp := planWithGovernance(t, "standard", &dsl.ApprovalSpec{
		Approvers:        []string{"alice", "bob"},
		MinApprovers:     1,
		ExcludeInitiator: true,
	})
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "self-approval"})
	require.NoError(t, err)
	_, err = svc.PlanChange(ContextWithActor(ctx, "alice"), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	_, err = svc.ApproveChange(ContextWithActor(ctx, "alice"), &pb.ApproveRequest{
		ChangeId: created.GetId(), Approver: "alice",
	})
	require.Error(t, err, "the initiator's own vote must not count")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "independent review")

	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	assert.NotEqual(t, "approved", run.Status, "a refused vote must not settle the run")

	// A genuinely independent reviewer still works — the rule is
	// independence, not a blanket denial.
	_, err = svc.ApproveChange(ContextWithActor(ctx, "bob"), &pb.ApproveRequest{
		ChangeId: created.GetId(), Approver: "bob",
	})
	require.NoError(t, err)
}

// TestPlanChange_RefusesUnsatisfiableIndependentQuorum pins the earlier
// refusal: when independence shrinks the eligible set below the tier's
// quorum, planning fails instead of handing out a chain nobody can close.
func TestPlanChange_RefusesUnsatisfiableIndependentQuorum(t *testing.T) {
	ctx := context.Background()
	sp := planWithGovernance(t, "high", &dsl.ApprovalSpec{
		Approvers: []string{"alice", "bob"}, MinApprovers: 2,
	})
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "only-author-eligible"})
	require.NoError(t, err)

	_, err = svc.PlanChange(ContextWithActor(ctx, "alice"), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.Error(t, err, "high forces the author out, leaving 1 of the 2 required voters")
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "under-provisioned")

	rows, err := store.ListApprovals(ctx, state.ApprovalFilter{RunID: created.GetId()})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func TestApproveChange_VoteBoundToSubjectNotToClaimedName(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "identity-forgery"})
	require.NoError(t, err)

	// bob, authenticated as bob, tries to record alice's approval.
	_, err = svc.ApproveChange(ContextWithActor(ctx, "bob"), &pb.ApproveRequest{
		ChangeId: created.GetId(), Approver: "alice",
	})
	require.Error(t, err, "a claimed approver may not override the credential")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "does not match the authenticated subject")

	// A caller with no identity at all is refused before any state read.
	_, err = svc.ApproveChange(ctx, &pb.ApproveRequest{ChangeId: created.GetId()})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// Nothing was recorded under either name.
	audits, err := store.ListAudits(ctx, state.AuditFilter{RunID: created.GetId(), Action: "approve"})
	require.NoError(t, err)
	assert.Empty(t, audits, "refused votes must not leave approval evidence behind")
}

// TestApproveChangeOverLegacyTokenUsesRealChain drives the RPC through the
// server's interceptor chain (recovery → logging → auth) rather than
// calling the service in process, so it covers the path a remote caller
// actually takes.
func TestApproveChangeOverLegacyTokenUsesRealChain(t *testing.T) {
	const legacyToken = "shared-deployment-token"
	srv, _ := startTestServerWithAllServices(t, WithAuthToken(legacyToken))
	conn := newInsecureClient(t, srv.Addr())
	client := pb.NewChangeServiceClient(conn)
	ctx := withAuthCtx(context.Background(), legacyToken)

	created, err := client.CreateChange(ctx, &pb.CreateChangeRequest{Label: "legacy-token"})
	require.NoError(t, err)

	_, err = client.ApproveChange(ctx, &pb.ApproveRequest{
		ChangeId: created.GetId(), Approver: "alice",
	})
	require.Error(t, err,
		"a shared token authenticates the deployment, not alice: it must not be able to approve")
	assert.Equal(t, codes.Unauthenticated, status.Code(err))

	// The same call with a named credential succeeds, so the refusal is
	// about identity and not about the RPC being unavailable.
	namedSrv, _ := startTestServerWithAllServices(t,
		WithAuthTokens([]TokenIdentity{{Token: "alice-token", Subject: "alice"}}))
	namedCtx := withAuthCtx(context.Background(), "alice-token")
	nc := pb.NewChangeServiceClient(newInsecureClient(t, namedSrv.Addr()))
	ncreated, err := nc.CreateChange(namedCtx, &pb.CreateChangeRequest{Label: "named-token"})
	require.NoError(t, err)
	_, err = nc.ApproveChange(namedCtx, &pb.ApproveRequest{
		ChangeId: ncreated.GetId(), Approver: "alice",
	})
	require.NoError(t, err, "a named subject may approve")
}

// TestPlanChangeRecordsTierWithoutApprovalService pins the hole where an
// irreversible plan in a status-only deployment (engine wired, no approval
// service) kept whatever priority the client had sent in the shared
// ApprovalLevel column, so the apply-time tier gate never saw "high".
func TestPlanChangeRecordsTierWithoutApprovalService(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{
		plan:   &pb.Plan{ChangeId: "x"},
		stored: planWithGovernance(t, "high", nil),
	}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())

	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "irreversible", Priority: "normal"})
	require.NoError(t, err)
	_, err = svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	require.Nil(t, svc.approval, "this deployment wires no approval service")

	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "high", run.ApprovalLevel,
		"the irreversible floor must be recorded even with no approval chain")

	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{
		ChangeId: created.GetId(), AutoApprove: true,
	})
	require.Error(t, err, "an irreversible change must not be auto-approvable")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, int32(0), atomic.LoadInt32(&rec.runCalled), "the engine must not run")
}

func TestRESTApproveRejectsNameThatIsNotTheBearerSubject(t *testing.T) {
	cfg := ServeGatewayConfig{AuthTokens: []TokenIdentity{
		{Token: "alice-token", Subject: "alice"},
		{Token: "bob-token", Subject: "bob"},
	}}
	gw, srv, store := startTestGatewayFull(t, cfg)

	created, err := gw.change.CreateChange(ContextWithActor(context.Background(), "carol"),
		&pb.CreateChangeRequest{Label: "rest-forgery"})
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost,
		srv.URL+"/changes/"+created.GetId()+"/approve",
		strings.NewReader(`{"approver":"alice","comment":"forged"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	// bob holds a valid credential and names alice in the body.
	req.Header.Set("Authorization", "Bearer bob-token")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"REST must not let a token holder record someone else's approval")

	audits, err := store.ListAudits(context.Background(),
		state.AuditFilter{RunID: created.GetId(), Action: "approve"})
	require.NoError(t, err)
	assert.Empty(t, audits, "the forged vote left no approval trail")
}

var _ = approval.LevelHigh // keep the approval import meaningful for readers
