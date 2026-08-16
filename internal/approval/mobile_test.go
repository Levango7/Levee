package approval

import (
	"testing"
	"time"

	"github.com/nexus/levee/internal/push"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---------------------------------------------------------------

// newMobileService builds a MobileApprovalService backed by a fresh mockStore
// and a real DeepLinkGenerator. The push manager is nil by default; tests
// that need push delivery should pass a non-nil manager via withPushManager.
func newMobileService(t *testing.T) (*MobileApprovalService, *Service, *mockStore) {
	t.Helper()
	store := newMockStore()
	svc := NewService(store)
	deeplink := push.NewDeepLinkGenerator("levee", "")
	mobile := NewMobileApprovalService(svc, nil, deeplink)
	return mobile, svc, store
}

// newMobileServiceWithPush builds a MobileApprovalService with a real
// PushManager (no APNs / FCM clients) so that device registration works
// but actual send calls are no-ops.
func newMobileServiceWithPush(t *testing.T) (*MobileApprovalService, *Service, *mockStore, *push.PushManager, *push.DeepLinkGenerator) {
	t.Helper()
	store := newMockStore()
	svc := NewService(store)
	deeplink := push.NewDeepLinkGenerator("levee", "")
	pm := push.NewPushManager(nil, nil)
	mobile := NewMobileApprovalService(svc, pm, deeplink)
	return mobile, svc, store, pm, deeplink
}

// --- NewMobileApprovalService ----------------------------------------------

func TestNewMobileApprovalService_NilDepsAllowed(t *testing.T) {
	mobile := NewMobileApprovalService(nil, nil, nil)
	assert.NotNil(t, mobile)
}

// --- RequestApproval -------------------------------------------------------

func TestMobileRequestApproval_CreatesApprovalAndSkipsPush(t *testing.T) {
	mobile, svc, store := newMobileService(t)
	ctx := bgCtx()

	err := mobile.RequestApproval(ctx, "run-1", "alice")
	require.NoError(t, err)

	// Approval was created in the store.
	pending, err := store.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "run-1", pending[0].RunID)
	assert.Equal(t, "alice", pending[0].Approvers[0])

	// No push manager configured; the call still succeeded.
	_ = svc
}

func TestMobileRequestApproval_EmptyRunID(t *testing.T) {
	mobile, _, _ := newMobileService(t)
	err := mobile.RequestApproval(bgCtx(), "", "alice")
	assert.ErrorIs(t, err, ErrEmptyRunID)
}

func TestMobileRequestApproval_EmptyUserID(t *testing.T) {
	mobile, _, _ := newMobileService(t)
	err := mobile.RequestApproval(bgCtx(), "run-1", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty user id")
}

func TestMobileRequestApproval_WithPushManager(t *testing.T) {
	mobile, _, store, pm, _ := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// Register a device so SendToUser does not return ErrDeviceNotFound.
	// The push manager has nil APNs/FCM, so the actual send is a no-op
	// (logged as a warning). The approval should still be created.
	require.NoError(t, pm.RegisterDevice("alice", "ios-token", "ios"))

	err := mobile.RequestApproval(ctx, "run-1", "alice")
	require.NoError(t, err)

	pending, err := store.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestMobileRequestApproval_PushFailureDoesNotBlockApproval(t *testing.T) {
	mobile, _, store, _, _ := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// No device registered; SendToUser will return ErrDeviceNotFound, but
	// the approval creation should still succeed because push is best-effort.
	err := mobile.RequestApproval(ctx, "run-1", "alice")
	require.NoError(t, err)

	pending, err := store.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// --- ApproveViaDeepLink ----------------------------------------------------

func TestMobileApproveViaDeepLink_Success(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// Create an approval and generate a deep link for it.
	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, err := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	// Approve via the deep link.
	err = mobile.ApproveViaDeepLink(ctx, link.Token)
	require.NoError(t, err)

	// History recorded.
	hist, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, ActionApprove, hist[0].Action)
	assert.Equal(t, "run-1", hist[0].RunID)
	assert.Equal(t, "mobile", hist[0].Source)
}

func TestMobileApproveViaDeepLink_InvalidToken(t *testing.T) {
	mobile, _, _, _, _ := newMobileServiceWithPush(t)
	err := mobile.ApproveViaDeepLink(bgCtx(), "bogus-token")
	require.Error(t, err)
	assert.ErrorIs(t, err, push.ErrInvalidToken)
}

func TestMobileApproveViaDeepLink_ExpiredToken(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	deeplink.SetTTL(1 * time.Millisecond)
	link, err := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)
	err = mobile.ApproveViaDeepLink(ctx, link.Token)
	require.Error(t, err)
	assert.ErrorIs(t, err, push.ErrTokenExpired)
}

func TestMobileApproveViaDeepLink_SingleUse(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, err := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	require.NoError(t, mobile.ApproveViaDeepLink(ctx, link.Token))
	err = mobile.ApproveViaDeepLink(ctx, link.Token)
	require.Error(t, err)
}

func TestMobileApproveViaDeepLink_WrongAction(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, err := deeplink.GenerateRejectLink("run-1", "alice")
	require.NoError(t, err)

	err = mobile.ApproveViaDeepLink(ctx, link.Token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not approve")
}

func TestMobileApproveViaDeepLink_NoPendingApproval(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// Generate a link for a run that has no approval.
	link, err := deeplink.GenerateApprovalLink("run-no-approval", "alice")
	require.NoError(t, err)

	err = mobile.ApproveViaDeepLink(ctx, link.Token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pending approval")
}

// --- RejectViaDeepLink -----------------------------------------------------

func TestMobileRejectViaDeepLink_Success(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, err := deeplink.GenerateRejectLink("run-1", "alice")
	require.NoError(t, err)

	err = mobile.RejectViaDeepLink(ctx, link.Token)
	require.NoError(t, err)

	hist, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, ActionReject, hist[0].Action)
	assert.Equal(t, "mobile", hist[0].Source)
	assert.NotEmpty(t, hist[0].Reason)
}

func TestMobileRejectViaDeepLink_InvalidToken(t *testing.T) {
	mobile, _, _, _, _ := newMobileServiceWithPush(t)
	err := mobile.RejectViaDeepLink(bgCtx(), "bogus")
	require.Error(t, err)
	assert.ErrorIs(t, err, push.ErrInvalidToken)
}

func TestMobileRejectViaDeepLink_WrongAction(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, err := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, err)

	err = mobile.RejectViaDeepLink(ctx, link.Token)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not reject")
}

// --- GetApprovalHistory ----------------------------------------------------

func TestMobileGetApprovalHistory_Empty(t *testing.T) {
	mobile, _, _, _, _ := newMobileServiceWithPush(t)
	hist, err := mobile.GetApprovalHistory(bgCtx(), "alice")
	require.NoError(t, err)
	assert.Nil(t, hist)
}

func TestMobileGetApprovalHistory_MostRecentFirst(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// Two approvals, two decisions.
	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	require.NoError(t, mobile.RequestApproval(ctx, "run-2", "alice"))

	link1, _ := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, mobile.ApproveViaDeepLink(ctx, link1.Token))
	link2, _ := deeplink.GenerateRejectLink("run-2", "alice")
	require.NoError(t, mobile.RejectViaDeepLink(ctx, link2.Token))

	hist, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, hist, 2)
	// Most recent first: reject (run-2) was recorded after approve (run-1).
	assert.Equal(t, "run-2", hist[0].RunID)
	assert.Equal(t, ActionReject, hist[0].Action)
	assert.Equal(t, "run-1", hist[1].RunID)
	assert.Equal(t, ActionApprove, hist[1].Action)
}

func TestMobileGetApprovalHistory_CappedAtMaxEntries(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// Record maxHistoryEntries + 10 decisions.
	for i := 0; i < maxHistoryEntries+10; i++ {
		runID := "run-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
		require.NoError(t, mobile.RequestApproval(ctx, runID, "alice"))
		link, err := deeplink.GenerateApprovalLink(runID, "alice")
		require.NoError(t, err)
		require.NoError(t, mobile.ApproveViaDeepLink(ctx, link.Token))
	}
	hist, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	assert.Len(t, hist, maxHistoryEntries)
}

func TestMobileGetApprovalHistory_ReturnsCopy(t *testing.T) {
	mobile, _, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	require.NoError(t, mobile.RequestApproval(ctx, "run-1", "alice"))
	link, _ := deeplink.GenerateApprovalLink("run-1", "alice")
	require.NoError(t, mobile.ApproveViaDeepLink(ctx, link.Token))

	hist, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	hist[0].RunID = "mutated"

	// Original is unaffected.
	hist2, err := mobile.GetApprovalHistory(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, "run-1", hist2[0].RunID)
}

// --- Integration: full mobile approval flow --------------------------------

func TestMobileApprovalFlow_FullCycle(t *testing.T) {
	mobile, svc, _, _, deeplink := newMobileServiceWithPush(t)
	ctx := bgCtx()

	// 1. Request approval (creates pending + sends push with deeplink).
	require.NoError(t, mobile.RequestApproval(ctx, "run-42", "bob"))

	// 2. Find the pending approval.
	pending, err := svc.store.ListPending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	approvalID := pending[0].ID

	// 3. Generate a one-tap approve link and consume it.
	link, err := deeplink.GenerateApprovalLink("run-42", "bob")
	require.NoError(t, err)
	require.NoError(t, mobile.ApproveViaDeepLink(ctx, link.Token))

	// 4. Verify the approval is now approved.
	got, err := svc.Get(ctx, approvalID)
	require.NoError(t, err)
	assert.Equal(t, StatusApproved, got.Status)

	// 5. History shows the decision.
	hist, err := mobile.GetApprovalHistory(ctx, "bob")
	require.NoError(t, err)
	require.Len(t, hist, 1)
	assert.Equal(t, ActionApprove, hist[0].Action)
}
