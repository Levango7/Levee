package approval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexus/levee/internal/verify"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastApprover returns a transport that polls as fast as the test can drive it,
// so the suite does not sleep on the production interval.
func fastApprover(svc *Service) *GateApprover {
	g := NewGateApprover(svc)
	g.interval = time.Millisecond
	return g
}

// waitForRecord blocks until the gate's approval row exists.
func waitForRecord(t *testing.T, store *mockStore, id string) *Approval {
	t.Helper()
	var row *Approval
	require.Eventually(t, func() bool {
		row, _ = store.Get(context.Background(), id)
		return row != nil
	}, 2*time.Second, time.Millisecond, "gate approval %q was never opened", id)
	return row
}

func TestGateApproverReturnsTheApprovalDecision(t *testing.T) {
	svc, store := newService(t)
	g := fastApprover(svc)

	done := make(chan verify.HumanDecision, 1)
	errCh := make(chan error, 1)
	go func() {
		d, err := g.RequestAndWait(context.Background(), "run-1", "release-hold", "freeze")
		done <- d
		errCh <- err
	}()

	id := GateApprovalID("run-1", "release-hold")
	waitForRecord(t, store, id)
	require.NoError(t, svc.Approve(context.Background(), id, "alice"))

	select {
	case d := <-done:
		require.NoError(t, <-errCh)
		assert.True(t, d.Approved)
		assert.Equal(t, "alice", d.Approver, "the decision must carry who approved")
	case <-time.After(3 * time.Second):
		t.Fatal("transport never returned after the approval was recorded")
	}
}

func TestGateApproverReturnsARejection(t *testing.T) {
	svc, store := newService(t)
	g := fastApprover(svc)

	done := make(chan verify.HumanDecision, 1)
	go func() {
		d, _ := g.RequestAndWait(context.Background(), "run-2", "cutover", "")
		done <- d
	}()

	id := GateApprovalID("run-2", "cutover")
	waitForRecord(t, store, id)
	require.NoError(t, svc.Reject(context.Background(), id, "bob", "not now"))

	select {
	case d := <-done:
		assert.False(t, d.Approved, "a rejection must not report approval")
		assert.Equal(t, "bob", d.Approver, "a rejection names who refused it")
	case <-time.After(3 * time.Second):
		t.Fatal("transport never returned after the rejection was recorded")
	}
}

// The gate approval must be invisible to the change-approval machinery, or
// approving a run would settle its gates and vice versa. That is enforced by
// binding the record to a PlanHash no run can have.
func TestGateApproverOpensAnIsolatedRecord(t *testing.T) {
	svc, store := newService(t)
	g := fastApprover(svc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = g.RequestAndWait(ctx, "run-3", "gate-x", "") }()

	row := waitForRecord(t, store, GateApprovalID("run-3", "gate-x"))
	require.NotEmpty(t, row.PlanHash, "an empty PlanHash is the legacy marker and matches EVERY plan")
	assert.True(t, strings.HasPrefix(row.PlanHash, GatePlanMarkerPrefix), "got %q", row.PlanHash)
	assert.NotEqual(t, "v2:deadbeef", row.PlanHash, "a run's plan hash can never equal the gate marker")
}

func TestGateApproverHonoursCancellation(t *testing.T) {
	svc, store := newService(t)
	g := fastApprover(svc)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := g.RequestAndWait(ctx, "run-4", "gate-y", "")
		errCh <- err
	}()

	waitForRecord(t, store, GateApprovalID("run-4", "gate-y"))
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("transport ignored ctx cancellation")
	}
}

// A resumed gate (or an identical phase declaring the same name) re-attaches to
// the record it already opened rather than failing or stacking a second one.
func TestGateApproverReattachesToAnExistingRecord(t *testing.T) {
	svc, store := newService(t)
	g := fastApprover(svc)

	id := GateApprovalID("run-5", "gate-z")
	_, err := svc.Create(context.Background(), CreateRequest{
		ID: id, RunID: "run-5", Level: LevelStandard, MinApprovers: 1,
		PlanHash: GatePlanMarkerPrefix + "gate-z",
	})
	require.NoError(t, err)

	done := make(chan verify.HumanDecision, 1)
	go func() {
		d, _ := g.RequestAndWait(context.Background(), "run-5", "gate-z", "")
		done <- d
	}()

	require.NoError(t, svc.Approve(context.Background(), id, "carol"))
	select {
	case d := <-done:
		assert.True(t, d.Approved)
		assert.Equal(t, "carol", d.Approver)
	case <-time.After(3 * time.Second):
		t.Fatal("re-attached transport never returned")
	}
	_ = store
}
