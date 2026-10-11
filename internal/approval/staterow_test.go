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
