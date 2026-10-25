// approval_guard_test.go pins the adapter that brings the permission matrix
// to approval votes cast directly against the domain service. The authz
// package owns the decision rule; what matters here is that a real vote is
// judged in the CHANGE's environment, that the no-matrix posture admits
// everything (the compatibility promise), and that a store that cannot
// answer fails closed instead of guessing.
package grpc

import (
	"context"
	"strings"
	"testing"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGuardVoteFixture(t *testing.T) (*ApprovalDecisionGuard, state.Store) {
	t.Helper()
	store := newTestStore(t)
	guard := &ApprovalDecisionGuard{
		Auth:  newPolicyAuthorizer(t), // fixture: alice may approve in dev; prod is view-only; mallory unregistered
		Store: store,
	}
	return guard, store
}

// A vote the matrix grants (alice, approve, dev) passes.
func TestApprovalDecisionGuardAllowsMatrixGrantedVote(t *testing.T) {
	guard, store := newGuardVoteFixture(t)
	runID := seedRunIn(t, store, "alice", "dev")

	err := guard.AuthorizeDecision(context.Background(), "alice", approval.ActionApprove, &approval.Approval{RunID: runID})
	require.NoError(t, err)
}

// The same subject, same action, an environment that only grants view: the
// vote is refused, and the refusal carries the operator-facing triple plus
// the `authz explain` pointer, because a bare "denied" is not actionable.
func TestApprovalDecisionGuardDeniesVoteInUngrantedEnvironment(t *testing.T) {
	guard, store := newGuardVoteFixture(t)
	runID := seedRunIn(t, store, "alice", "prod")

	err := guard.AuthorizeDecision(context.Background(), "alice", approval.ActionApprove, &approval.Approval{RunID: runID})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, `subject "alice"`)
	assert.Contains(t, msg, `env "prod"`)
	assert.Contains(t, msg, `action "approve"`)
	assert.Contains(t, msg, "levee authz explain")
}

// An unregistered voter is refused even in an environment they could
// otherwise reach — registration is the floor, exactly as on the RPC path.
func TestApprovalDecisionGuardDeniesUnregisteredSubject(t *testing.T) {
	guard, store := newGuardVoteFixture(t)
	runID := seedRunIn(t, store, "alice", "dev")

	err := guard.AuthorizeDecision(context.Background(), "mallory", approval.ActionReject, &approval.Approval{RunID: runID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered")
}

// No authorizer configured: the guard admits everything, which is the same
// posture authorizeResource gives a deployment without permissions.yaml.
func TestApprovalDecisionGuardNilAuthAdmits(t *testing.T) {
	_, store := newGuardVoteFixture(t)
	guard := &ApprovalDecisionGuard{Auth: nil, Store: store} // no matrix loaded
	require.NoError(t, guard.AuthorizeDecision(context.Background(), "anyone", approval.ActionApprove, &approval.Approval{RunID: "run-x"}))

	// The nil receiver and the nil record are the same "nothing to judge"
	// family: admitted, because there is no policy that said otherwise.
	var nilGuard *ApprovalDecisionGuard
	require.NoError(t, nilGuard.AuthorizeDecision(context.Background(), "anyone", approval.ActionApprove, nil))
}

// An approval whose run is gone is judged in the deployment's default
// environment — the same answer envOf gives authorizeResource for a runless
// resource. It is not a bypass: the default env is still an env the matrix
// decides on.
func TestApprovalDecisionGuardMissingRunJudgedInDefaultEnv(t *testing.T) {
	guard, _ := newGuardVoteFixture(t) // fixture authorizer defaults to dev

	// dev grants alice approve, so the vote passes on the default env.
	require.NoError(t, guard.AuthorizeDecision(context.Background(), "alice", approval.ActionApprove, &approval.Approval{RunID: "run-gone"}))

	// mallory is unregistered in any environment, so the same missing run
	// does not become an unjudged vote for him.
	err := guard.AuthorizeDecision(context.Background(), "mallory", approval.ActionApprove, &approval.Approval{RunID: "run-gone"})
	require.Error(t, err)
}

// A store that cannot answer fails closed: an unreadable run is not a
// policy grant.
func TestApprovalDecisionGuardFailsClosedOnStoreError(t *testing.T) {
	store := newTestStore(t)
	guard := &ApprovalDecisionGuard{
		Auth:  newPolicyAuthorizer(t),
		Store: &errorStore{Store: store, getRunErr: assert.AnError},
	}
	err := guard.AuthorizeDecision(context.Background(), "alice", approval.ActionApprove, &approval.Approval{RunID: "run-1"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "resolve run"), "the refusal should name the store failure, not a policy reason")
}
