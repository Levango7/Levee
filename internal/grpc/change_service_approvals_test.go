// change_service_approvals_test.go pins the approval-chain read path that this
// batch adds: without it, a pending change could be shown as "awaiting
// approval" and nothing else, and the entrap the roadmap's approval-centre row
// names -- a vote that cannot land because the quorum is short, or because the
// approver is the initiator, or because the plan moved -- looked identical to
// "nobody has approved yet".
//
// A chain is one record carrying who was asked, the quorum, and every decision;
// the tests assert those four things specifically, because they are what the
// card has to show and what the internal-only code never exposed.
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// newApprovalSvc builds a change service with an authorizer over the shared
// policy fixture (alice/operator, carol/viewer, mallory unregistered; prod is
// view-only for the team, dev allows plan).
func newApprovalSvc(t *testing.T) (*ChangeService, state.Store) {
	t.Helper()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	return svc, store
}

// createChain kicks off one chain over change id and returns its id.
func createChain(t *testing.T, store state.Store, id string, approvers []string, minApprovers int) string {
	t.Helper()
	as := approval.NewService(&kickoffStoreAdapter{store: store})
	_, err := as.Create(context.Background(), approval.CreateRequest{
		RunID: id, Level: approval.LevelStandard, Approvers: approvers,
		MinApprovers: minApprovers, ExpiresAt: time.Now().UTC().Add(time.Hour),
		Initiator: "alice", ExcludeInitiator: true,
	})
	require.NoError(t, err)
	rows, err := store.ListApprovals(context.Background(), state.ApprovalFilter{RunID: id})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].ID
}

func TestGetApprovals_ExposesWhatTheCardNeeds(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	// Quorum 2 over three approvers, and ONE vote cast: the quorum is short, so
	// the chain is still pending -- which is exactly the state that used to be
	// indistinguishable from "nobody has approved yet".
	chainID := createChain(t, store, id, []string{"bob", "carol", "dave"}, 2)
	as := approval.NewService(&kickoffStoreAdapter{store: store})
	require.NoError(t, as.Approve(ctx, chainID, "bob"))

	resp, err := svc.GetApprovals(ContextWithActor(ctx, "carol"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	require.Len(t, resp.GetApprovals(), 1, "one chain per change in this flow")

	chain := resp.GetApprovals()[0]
	assert.Equal(t, chainID, chain.GetId())
	assert.Equal(t, approval.LevelStandard, chain.GetLevel())
	assert.Equal(t, string(approval.StatusPending), chain.GetStatus(),
		"a partial quorum must not read as a decided chain")
	assert.Equal(t, []string{"bob", "carol", "dave"}, chain.GetApprovers())
	assert.Equal(t, int32(2), chain.GetMinApprovers(), "the card must show the quorum, not a bare count")
	assert.True(t, chain.GetExcludeInitiator(), "the initiator being barred is exactly the entrap worth showing")
	assert.Equal(t, "alice", chain.GetInitiator())
	assert.NotZero(t, chain.GetExpiresAt())

	// The one vote, readable with its time.
	require.Len(t, chain.GetDecisions(), 1)
	assert.Equal(t, "bob", chain.GetDecisions()[0].GetApprover())
	assert.Equal(t, approval.ActionApprove, chain.GetDecisions()[0].GetAction())
	assert.NotZero(t, chain.GetDecisions()[0].GetDecidedAt())
	assert.True(t, chain.GetLegacy(), "this chain has no plan hash, so it is a legacy record")

	// Who has NOT decided is the answer to "why is this still pending", and it
	// is the set difference, not a second list the server would have to invent.
	var pending []string
	decided := map[string]bool{}
	for _, d := range chain.GetDecisions() {
		decided[d.GetApprover()] = true
	}
	for _, a := range chain.GetApprovers() {
		if !decided[a] {
			pending = append(pending, a)
		}
	}
	assert.Equal(t, []string{"carol", "dave"}, pending)
}

// TestGetApprovals_ARejectionDecidesTheChainAndCarriesItsReason: a reject is a
// terminal decision for the chain, and the console has to show WHY, not just
// that the change died.
func TestGetApprovals_ARejectionDecidesTheChainAndCarriesItsReason(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	chainID := createChain(t, store, id, []string{"bob", "carol"}, 2)
	as := approval.NewService(&kickoffStoreAdapter{store: store})
	require.NoError(t, as.Reject(ctx, chainID, "bob", "窗口不合适，推到低峰期"))

	resp, err := svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	require.Len(t, resp.GetApprovals(), 1)
	assert.Equal(t, string(approval.StatusRejected), resp.GetApprovals()[0].GetStatus())
	require.Len(t, resp.GetApprovals()[0].GetDecisions(), 1)
	assert.Equal(t, approval.ActionReject, resp.GetApprovals()[0].GetDecisions()[0].GetAction())
	assert.Contains(t, resp.GetApprovals()[0].GetDecisions()[0].GetReason(), "低峰期")
}

// TestGetApprovals_ALegacyChainSaysSo: a chain with no plan hash still settles
// the run regardless of plan version. Rendering that as a blank would read as
// "this decision attests to nothing", which is the opposite of what it does.
// TestGetApprovals_StaleWhenTheRunMovedOn: a chain that bound to a plan hash
// which is no longer the run's is a verdict on a superseded plan. The
// approver (and anyone reading the card) must be told, because approving
// against an old plan is exactly the entrap this page exists to surface.
func TestGetApprovals_StaleWhenTheRunMovedOn(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	chainID := createChainWithPlan(t, store, id, "plan-old-1")

	// Same hash: the chain is the current plan — not stale.
	resp, err := svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	found := findChain(resp, chainID)
	require.NotNil(t, found)
	assert.False(t, found.GetStale(), "the bound plan is the run's plan")
	assert.Equal(t, "plan-old-1", found.GetPlanHash())

	// The run re-planned: the chain now attests to a superseded version.
	run, err := store.GetRun(ctx, id)
	require.NoError(t, err)
	run.PlanHash = "plan-new-2"
	require.NoError(t, store.UpdateRun(ctx, run))

	resp, err = svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	found = findChain(resp, chainID)
	require.NotNil(t, found)
	assert.True(t, found.GetStale(), "a bound plan that is no longer the run's must be flagged")
}

// TestGetApprovals_NoCurrentPlanIsNotStale: an empty run hash is the absence of
// a comparison, not a verdict: the run has not (re)planned yet.
func TestGetApprovals_NoCurrentPlanIsNotStale(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	createChainWithPlan(t, store, id, "plan-old-1")

	resp, err := svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	require.Len(t, resp.GetApprovals(), 1)
	assert.False(t, resp.GetApprovals()[0].GetStale())
	assert.False(t, resp.GetApprovals()[0].GetLegacy(), "a bound plan is not a legacy chain")
}

func createChainWithPlan(t *testing.T, store state.Store, id, planHash string) string {
	t.Helper()
	as := approval.NewService(&kickoffStoreAdapter{store: store})
	_, err := as.Create(context.Background(), approval.CreateRequest{
		RunID: id, Level: approval.LevelStandard, Approvers: []string{"bob"},
		MinApprovers: 1, ExpiresAt: time.Now().UTC().Add(time.Hour),
		PlanHash: planHash, Initiator: "alice", ExcludeInitiator: true,
	})
	require.NoError(t, err)
	rows, err := store.ListApprovals(context.Background(), state.ApprovalFilter{RunID: id})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0].ID
}

func findChain(resp *pb.GetApprovalsResponse, chainID string) *pb.ApprovalInfo {
	for _, c := range resp.GetApprovals() {
		if c.GetId() == chainID {
			return c
		}
	}
	return nil
}

func TestGetApprovals_ALegacyChainSaysSo(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	require.NotEmpty(t, createChain(t, store, id, []string{"bob"}, 1))

	resp, err := svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	require.Len(t, resp.GetApprovals(), 1)
	assert.True(t, resp.GetApprovals()[0].GetLegacy())
	assert.Empty(t, resp.GetApprovals()[0].GetPlanHash())
}

func TestGetApprovals_GatedLikeEveryOtherRead(t *testing.T) {
	ctx := context.Background()

	// The team may only VIEW prod: reading a prod chain is inside `view`, so a
	// registered viewer (carol) may read it.
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	require.NotEmpty(t, createChain(t, store, id, []string{"bob"}, 1))
	resp, err := svc.GetApprovals(ContextWithActor(ctx, "carol"), &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	assert.Len(t, resp.GetApprovals(), 1)

	// The devViewerPolicy fixture is the shape a refusal needs: dave is
	// registered and may read dev only, and prod is reachable (so the gate
	// judges it) but grants nothing.
	svc2, store2 := newTestChangeService(t)
	svc2.WithAuthorizer(newDevViewerAuthorizer(t))
	prodID := seedRunIn(t, store2, "alice", "prod")
	require.NotEmpty(t, createChain(t, store2, prodID, []string{"bob"}, 1))
	devID := seedRunIn(t, store2, "alice", "dev")
	require.NotEmpty(t, createChain(t, store2, devID, []string{"bob"}, 1))

	_, err = svc2.GetApprovals(ContextWithActor(ctx, "dave"), &pb.GetApprovalsRequest{ChangeId: prodID})
	require.Error(t, err, "a caller who may not see the environment may not read its approvals")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "levee authz explain")

	ok, err := svc2.GetApprovals(ContextWithActor(ctx, "dave"), &pb.GetApprovalsRequest{ChangeId: devID})
	require.NoError(t, err)
	assert.Len(t, ok.GetApprovals(), 1)
}

func TestGetApprovals_RefusesBadIds(t *testing.T) {
	svc, _ := newApprovalSvc(t)
	_, err := svc.GetApprovals(context.Background(), &pb.GetApprovalsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = svc.GetApprovals(ContextWithActor(context.Background(), "alice"),
		&pb.GetApprovalsRequest{ChangeId: "run-does-not-exist"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// TestGetApprovals_WithoutAPolicy: every other read path answers when no matrix
// is configured; this one must not become the read that starts refusing.
func TestGetApprovals_WithoutAPolicy(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	id := seedRunIn(t, store, "alice", "prod")
	resp, err := svc.GetApprovals(ctx, &pb.GetApprovalsRequest{ChangeId: id})
	require.NoError(t, err)
	assert.Empty(t, resp.GetApprovals(), "no policy and no rows: the answer is an empty chain")
}

// TestGetApprovals_AnUnreadableRowIsNamed: a chain whose comment is not the JSON
// this repo writes must fail with its id -- reported as an empty chain it would
// be hidden, and skipped silently it would hide the chains around it.
func TestGetApprovals_AnUnreadableRowIsNamed(t *testing.T) {
	ctx := context.Background()
	svc, store := newApprovalSvc(t)
	id := seedRunIn(t, store, "alice", "prod")
	require.NoError(t, store.CreateApproval(ctx, &state.Approval{
		ID:    "ap-corrupt",
		RunID: id,
		Level: approval.LevelStandard,
		// A comment a human typed in the console, not the stored JSON.
		Comment: "请 bob 和 carol 都确认一下",
	}))

	_, err := svc.GetApprovals(ContextWithActor(ctx, "alice"), &pb.GetApprovalsRequest{ChangeId: id})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ap-corrupt", "the failure must name the row it could not read")
}
