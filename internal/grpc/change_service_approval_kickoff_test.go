// change_service_approval_kickoff_test.go pins the R4 approval routing:
// PlanChange starts the approval chain (previously nobody created the
// pending record the ApproveChange flow needs), the tier is
// max(workflow declaration, plan risk floor) — the floor can raise but
// never lower — and a re-plan supersedes the earlier pending record.

package grpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

// kickoffStoreAdapter adapts the state store to approval.Store for the
// kickoff tests. The kickoff path only exercises Create (plus the
// service's own read-modify-write through Get/UpdateIfPending during
// supersession); decisions flows stay out of scope here. Approvers and
// the quorum are serialised into Comment so assertions can verify the
// routing inputs even though the state row has no dedicated columns.
type kickoffStoreAdapter struct {
	store state.Store
}

// kickoffMeta is the Comment JSON schema shared by Create/Get/Update:
// the state row has no columns for these, so they round-trip through
// Comment. ALL fields must survive (Decisions included) or decide()'s
// read-modify-write loses votes between decisions.
type kickoffMeta struct {
	Approvers    []string            `json:"approvers,omitempty"`
	MinApprovers int                 `json:"min_approvers,omitempty"`
	Decisions    []approval.Decision `json:"decisions,omitempty"`
}

// encodeKickoffMeta serialises ap's chain state into the Comment blob.
func encodeKickoffMeta(ap *approval.Approval) string {
	raw, _ := json.Marshal(kickoffMeta{
		Approvers:    ap.Approvers,
		MinApprovers: ap.MinApprovers,
		Decisions:    ap.Decisions,
	})
	return string(raw)
}

func (a *kickoffStoreAdapter) Create(ctx context.Context, ap *approval.Approval) error {
	acted := ap.ExpiresAt
	return a.store.CreateApproval(ctx, &state.Approval{
		ID:        ap.ID,
		RunID:     ap.RunID,
		Level:     ap.Level,
		Status:    string(ap.Status),
		Comment:   encodeKickoffMeta(ap),
		TimeoutAt: &acted,
		PlanHash:  ap.PlanHash,
	})
}

func (a *kickoffStoreAdapter) Get(ctx context.Context, id string) (*approval.Approval, error) {
	row, err := a.store.GetApproval(ctx, id)
	if err != nil || row == nil {
		return nil, approval.ErrNotFound
	}
	out := &approval.Approval{
		ID:       row.ID,
		RunID:    row.RunID,
		Level:    row.Level,
		Status:   approval.Status(row.Status),
		PlanHash: row.PlanHash,
	}
	if row.TimeoutAt != nil {
		out.ExpiresAt = *row.TimeoutAt
	}
	if row.Comment != "" {
		var meta kickoffMeta
		if err := json.Unmarshal([]byte(row.Comment), &meta); err == nil {
			out.Approvers = meta.Approvers
			out.MinApprovers = meta.MinApprovers
			out.Decisions = meta.Decisions
		}
	}
	if out.MinApprovers == 0 {
		out.MinApprovers = 1
	}
	return out, nil
}

func (a *kickoffStoreAdapter) Update(ctx context.Context, ap *approval.Approval) error {
	acted := ap.ExpiresAt
	return a.store.UpdateApproval(ctx, &state.Approval{
		ID:        ap.ID,
		RunID:     ap.RunID,
		Level:     ap.Level,
		Status:    string(ap.Status),
		Comment:   encodeKickoffMeta(ap),
		TimeoutAt: &acted,
	})
}

func (a *kickoffStoreAdapter) UpdateIfPending(ctx context.Context, ap *approval.Approval) (bool, error) {
	row, err := a.store.GetApproval(ctx, ap.ID)
	if err != nil || row == nil {
		return false, approval.ErrNotFound
	}
	if row.Status != "pending" {
		return false, nil
	}
	acted := ap.ExpiresAt
	return true, a.store.UpdateApproval(ctx, &state.Approval{
		ID:        ap.ID,
		RunID:     ap.RunID,
		Level:     ap.Level,
		Status:    string(ap.Status),
		Comment:   encodeKickoffMeta(ap),
		TimeoutAt: &acted,
	})
}

func (a *kickoffStoreAdapter) ListPending(ctx context.Context) ([]*approval.Approval, error) {
	rows, err := a.store.ListApprovals(ctx, state.ApprovalFilter{Status: "pending"})
	if err != nil {
		return nil, err
	}
	out := make([]*approval.Approval, 0, len(rows))
	for _, r := range rows {
		out = append(out, &approval.Approval{
			ID: r.ID, RunID: r.RunID, Level: r.Level,
			Status: approval.Status(r.Status), MinApprovers: 1,
		})
	}
	return out, nil
}

// newKickoffService wires a service with a REAL approval service over
// the state store (the standard newTestChangeServiceWithEngine passes
// approval=nil, which would skip the kickoff entirely).
func newKickoffService(t *testing.T, engine *EngineAdapter) (*ChangeService, state.Store) {
	t.Helper()
	store := newTestStore(t)
	svc := NewChangeService(store, engine, approval.NewService(&kickoffStoreAdapter{store: store}), nil)
	return svc, store
}

// planWithFloor builds a StoredPlan whose artifact carries the given
// approval floor (simulating the risk assessor's stamp).
func planWithFloor(t *testing.T, floor string) *StoredPlan {
	t.Helper()
	p := testPlan()
	p.ApprovalFloor = floor
	p.RiskScore = 45
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return &StoredPlan{JSON: string(raw), Hash: plan.ComputeHash(p)}
}

func TestPlanChange_KicksOffApprovalChain(t *testing.T) {
	sp := planWithFloor(t, "high")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())
	created, err := svc.CreateChange(ContextWithActor(context.Background(), "alice"), &pb.CreateChangeRequest{
		Label: "kickoff",
	})
	require.NoError(t, err)

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId:    created.GetId(),
		TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	// The pending approval exists — the chain is finally started by
	// the system itself.
	approvals, err := store.ListApprovals(context.Background(), state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	require.Len(t, approvals, 1, "planning must kick off exactly one pending approval")
	assert.Equal(t, "high", approvals[0].Level, "floor high routes the tier to high")

	// The run's ApprovalLevel reflects the effective tier.
	run, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	assert.Equal(t, "high", run.ApprovalLevel)
}

func TestApprovalRouting_DeclaredEmergencyWinsOverHighFloor(t *testing.T) {
	// Workflow declares emergency (inline YAML); floor high ⇒ tier
	// emergency (the declaration can demand MORE review than the floor).
	inline := "name: wf\ntargets:\n  - name: web\n    hosts: [\"web-1\"]\nsteps:\n  - name: s\n    action: shell.exec\n    args:\n      cmd: uname\napproval:\n  level: emergency\n"
	sp := planWithFloor(t, "high")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())
	created, err := svc.CreateChange(ContextWithActor(context.Background(), "alice"), &pb.CreateChangeRequest{
		Label: "decl-em",
	})
	require.NoError(t, err)

	// Inject the inline workflow source.
	ctx := context.Background()
	run, err := store.GetRun(ctx, created.GetId())
	require.NoError(t, err)
	run.WorkflowName = inline
	require.NoError(t, store.UpdateRun(ctx, run))

	_, err = svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	approvals, err := store.ListApprovals(ctx, state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	require.Len(t, approvals, 1)
	assert.Equal(t, "emergency", approvals[0].Level, "max(declared emergency, floor high) = emergency")
}

func TestApprovalRouting_RePlanSupersedesPending(t *testing.T) {
	sp := planWithFloor(t, "high")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())
	created, err := svc.CreateChange(ContextWithActor(context.Background(), "alice"), &pb.CreateChangeRequest{
		Label: "replan",
	})
	require.NoError(t, err)

	ctx := context.Background()
	_, err = svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)
	_, err = svc.PlanChange(ctx, &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	pending, err := store.ListApprovals(ctx, state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	require.Len(t, pending, 1, "exactly one pending row after re-plan")

	expired, err := store.ListApprovals(ctx, state.ApprovalFilter{
		RunID: created.GetId(), Status: "expired",
	})
	require.NoError(t, err)
	require.Len(t, expired, 1, "the superseded row is expired, not deleted")
}

func TestKickoffSkippedWithoutApprovalService(t *testing.T) {
	// Status-only deployment (no approval service): kickoff is a no-op
	// and planning still succeeds.
	sp := planWithFloor(t, "high")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newTestChangeServiceWithEngine(t, engine.adapter())
	created, err := svc.CreateChange(ContextWithActor(context.Background(), "alice"), &pb.CreateChangeRequest{
		Label: "no-appr",
	})
	require.NoError(t, err)

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	approvals, err := store.ListApprovals(context.Background(), state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	assert.Empty(t, approvals)
}

func TestKickoffDefaultsToCreatorWhenNoApproversDeclared(t *testing.T) {
	sp := planWithFloor(t, "standard")
	engine := &recordingEngine{plan: &pb.Plan{ChangeId: "x"}, stored: sp}
	svc, store := newKickoffService(t, engine.adapter())
	created, err := svc.CreateChange(ContextWithActor(context.Background(), "bob"), &pb.CreateChangeRequest{
		Label: "creator-default",
	})
	require.NoError(t, err)

	_, err = svc.PlanChange(context.Background(), &pb.PlanChangeRequest{
		ChangeId: created.GetId(), TargetHosts: []string{"web-1"},
	})
	require.NoError(t, err)

	approvals, err := store.ListApprovals(context.Background(), state.ApprovalFilter{
		RunID: created.GetId(), Status: "pending",
	})
	require.NoError(t, err)
	require.Len(t, approvals, 1)
	assert.Equal(t, "standard", approvals[0].Level)
	// 24h expiry was set on the created record.
	require.NotNil(t, approvals[0].TimeoutAt)
	assert.WithinDuration(t, time.Now().UTC().Add(24*time.Hour), *approvals[0].TimeoutAt, 5*time.Minute)
}
