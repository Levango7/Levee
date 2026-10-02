// The post-create observer slot used to be last-write-wins: serve gives it
// to the Jira mirror, so wiring a second mirror (the ChatOps approval
// bridge) would have SILENTLY DISPLACED Jira. These tests pin the fan-out
// on the ChangeService side, and that one misbehaving mirror neither kills
// the kickoff nor blocks the healthy ones.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithApprovalCreateObserver_FanOutKeepsEveryInstall(t *testing.T) {
	sp := planWithFloor(t, "standard")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, _ := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(context.Background(), "bob"), &pb.CreateChangeRequest{
		Label: "obs-fanout",
	})
	require.NoError(t, err)

	var fired []string
	svc.WithApprovalCreateObserver(func(a *approval.Approval) { fired = append(fired, "first") })
	svc.WithApprovalCreateObserver(func(a *approval.Approval) { fired = append(fired, "second") })

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "second"}, fired,
		"both observers must fire — fan-out, not displacement")
}

func TestWithApprovalCreateObserver_NilRemovesAll(t *testing.T) {
	sp := planWithFloor(t, "standard")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, _ := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(context.Background(), "bob"), &pb.CreateChangeRequest{
		Label: "obs-nil",
	})
	require.NoError(t, err)

	fired := 0
	svc.WithApprovalCreateObserver(func(a *approval.Approval) { fired++ })
	svc.WithApprovalCreateObserver(nil)

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, fired, "nil must remove every installed observer")
}

func TestWithApprovalCreateObserver_PanicDoesNotBreakKickoff(t *testing.T) {
	sp := planWithFloor(t, "standard")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())

	created, err := svc.CreateChange(ContextWithActor(context.Background(), "bob"), &pb.CreateChangeRequest{
		Label: "obs-panic",
	})
	require.NoError(t, err)

	healthy := 0
	svc.WithApprovalCreateObserver(func(a *approval.Approval) { panic("mirror on fire") })
	svc.WithApprovalCreateObserver(func(a *approval.Approval) { healthy++ })

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err, "one panicking mirror must not fail the kickoff")
	assert.Equal(t, 1, healthy, "the healthy mirror must still fire after the panicking one")

	approvals, err := store.ListApprovals(context.Background(), state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	assert.Len(t, approvals, 1, "the approval record is durable regardless of mirror behaviour")
}
