// staterow_test.go pins the row mapping that two surfaces used to carry their
// own copies of (cmd/levee and internal/grpc's tests). The mapping is storable
// format, so a change here silently rewrites how every existing approval row
// reads; these cases fix the shape.
package approval

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/state"
)

func TestStateApproval_RoundTrip(t *testing.T) {
	decided := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	a := &Approval{
		ID: "app-1", RunID: "run-1", Level: LevelHigh, Status: StatusApproved,
		Approvers: []string{"alice", "bob", "carol"}, MinApprovers: 2,
		Decisions: []Decision{
			{Approver: "bob", Action: ActionApprove, At: decided},
			{Approver: "carol", Action: ActionReject, Reason: "窗口不合适", At: decided.Add(time.Second)},
		},
		CreatedAt: decided.Add(-time.Hour), ExpiresAt: decided.Add(time.Hour),
		PlanHash: "plan-hash-1", Initiator: "alice", ExcludeInitiator: true,
	}
	sa, err := StateApproval(a)
	require.NoError(t, err)
	// The derived columns are what a column-only reader sees: last decider.
	assert.Equal(t, "carol", sa.Approver)
	require.NotNil(t, sa.ActedAt)
	assert.True(t, sa.ActedAt.Equal(decided.Add(time.Second)))

	back, err := ApprovalFromState(sa)
	require.NoError(t, err)
	assert.Equal(t, a.Approvers, back.Approvers)
	assert.Equal(t, a.MinApprovers, back.MinApprovers)
	assert.Equal(t, a.Decisions, back.Decisions)
	assert.Equal(t, a.Initiator, back.Initiator)
	assert.True(t, a.ExcludeInitiator)
	assert.Equal(t, "plan-hash-1", back.PlanHash)
}

func TestApprovalFromState_EmptyCommentIsAnEmptyChain(t *testing.T) {
	// A row written before the extra-JSON encoding, or by a writer that left
	// the comment blank, decodes to "no approvers, no decisions" rather than an
	// error: refusing one row would hide the chains around it.
	a, err := ApprovalFromState(&state.Approval{ID: "x", RunID: "r", Level: LevelStandard, Status: string(StatusPending)})
	require.NoError(t, err)
	assert.Empty(t, a.Approvers)
	assert.Empty(t, a.Decisions)
	assert.Equal(t, 0, a.MinApprovers)
}

func TestApprovalFromState_UnparsableCommentIsReported(t *testing.T) {
	_, err := ApprovalFromState(&state.Approval{ID: "x", RunID: "r", Comment: "not json"})
	require.Error(t, err, "a corrupt row must be identified, not silently read as empty")
	assert.Contains(t, err.Error(), "unparsable metadata")
}

func TestMarkSuperseded_KeepsTheChainReadable(t *testing.T) {
	// Regression: the supersession path used to write "superseded by re-plan"
	// straight into `comment`, which made ApprovalFromState fail — the expired
	// row lost its approvers and decisions on every read path.
	a := &Approval{
		ID: "app-1", RunID: "run-1", Level: LevelHigh, Status: StatusPending,
		Approvers: []string{"bob", "carol"}, MinApprovers: 2,
		CreatedAt: time.Now().UTC(), PlanHash: "plan-1",
	}
	sa, err := StateApproval(a)
	require.NoError(t, err)

	require.NoError(t, MarkSuperseded(sa, "superseded by re-plan"))
	sa.Status = string(StatusExpired)

	back, err := ApprovalFromState(sa)
	require.NoError(t, err, "a superseded row must stay readable")
	assert.Equal(t, []string{"bob", "carol"}, back.Approvers)
	assert.Equal(t, 2, back.MinApprovers)
	assert.Equal(t, "superseded by re-plan", back.SupersededReason)
	assert.Equal(t, StatusExpired, back.Status)
}

func TestMarkSuperseded_ToleratesLegacyEmptyComment(t *testing.T) {
	// A row with no chain JSON must still be markable: the reason is carried
	// inside a freshly-marshalled blob rather than dropped.
	sa := &state.Approval{ID: "legacy", RunID: "r", Status: "expired"}
	require.NoError(t, MarkSuperseded(sa, "superseded by re-plan"))
	back, err := ApprovalFromState(sa)
	require.NoError(t, err)
	assert.Empty(t, back.Approvers)
	assert.Equal(t, "superseded by re-plan", back.SupersededReason)
}

func TestMarkSuperseded_RefusesToClobberCorruptMetadata(t *testing.T) {
	sa := &state.Approval{ID: "x", Comment: "not json"}
	err := MarkSuperseded(sa, "reason")
	require.Error(t, err)
	assert.Equal(t, "not json", sa.Comment, "a corrupt blob is reported, not overwritten")
	err = MarkSuperseded(nil, "reason")
	require.Error(t, err)
}
