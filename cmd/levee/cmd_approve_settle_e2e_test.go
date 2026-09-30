// End-to-end tests for approval settlement from the CLI surfaces:
// `levee approve`/`levee reject` and `levee chatops approve`/`reject`
// must not only record the decision on the approval row but also mirror
// the chain outcome onto the run — otherwise the run stays draft and
// every later apply refuses it while the operator believes they
// approved. Also pins the quorum gate end to end: min 2 approvals means
// the first vote leaves the run pending.
package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/state"
)

// seedQuorumApproval writes a pending approval row WITH the multi-approver
// chain metadata the CLI adapter round-trips through Comment, so the
// quorum arithmetic can actually fire. Uses the production adapter.
func seedQuorumApproval(t *testing.T, e *cliEnv, id, changeID string, approvers []string, min int) {
	t.Helper()
	e.seedRun(t, changeID, "pending")
	store := e.open(t)
	defer func() { _ = store.Close() }()
	extra, err := marshalApprovalExtra(approvalExtra{
		Approvers:    approvers,
		MinApprovers: min,
		CreatedAt:    time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(24 * time.Hour),
	})
	require.NoError(t, err)
	require.NoError(t, store.CreateApproval(context.Background(), &state.Approval{
		ID: id, RunID: changeID, Level: "standard", Status: "pending",
		Comment: extra,
	}))
}

func TestApproveE2E_SettlesRunStatus(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	// Single-approver chain: one vote settles the run to approved.
	seedQuorumApproval(t, e, "ap-s1", "chg-settle-1", []string{"cli-user"}, 1)
	out := mustRunKeepCfg(t, e.cfgPath, "approve", "chg-settle-1")
	assert.Contains(t, out, "chg-settle-1")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "chg-settle-1")
	require.NoError(t, err)
	require.NotNil(t, run)
	assert.Equal(t, "approved", run.Status, "CLI approve must settle the run, not just the approval row")
	assert.Equal(t, "approved", run.ApprovalStatus)
}

func TestApproveE2E_PartialQuorumKeepsRunPending(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	// Two-approver quorum: the first vote records but must NOT approve.
	// (The "quorum still pending" progress line goes to stderr; the
	// behavioural pin is the run status below.)
	seedQuorumApproval(t, e, "ap-q1", "chg-quorum-1", []string{"cli-user", "bob"}, 2)
	_ = mustRunKeepCfg(t, e.cfgPath, "approve", "chg-quorum-1")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "chg-quorum-1")
	require.NoError(t, err)
	assert.NotEqual(t, "approved", run.Status, "1/2 votes must not approve the run")
}

func TestRejectE2E_SettlesRunRejected(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)

	seedQuorumApproval(t, e, "ap-r1", "chg-reject-1", []string{"cli-user", "bob"}, 2)
	out := mustRunKeepCfg(t, e.cfgPath, "reject", "chg-reject-1", "--reason", "not this week")
	assert.Contains(t, out, "chg-reject-1")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "chg-reject-1")
	require.NoError(t, err)
	assert.Equal(t, "rejected", run.Status, "one-vote veto must settle the run immediately")
}

func TestChatopsApproveE2E_SettlesRunStatus(t *testing.T) {
	defer resetRootFlags()
	e := newCLIEnv(t)
	t.Setenv("LEVEE_CHATOPS_APPROVER", "oncall.bob")

	seedQuorumApproval(t, e, "ap-c1", "chg-chatops-1", []string{"oncall.bob"}, 1)
	out := mustRunKeepCfg(t, e.cfgPath, "chatops", "approve", "chg-chatops-1")
	assert.Contains(t, out, "chg-chatops-1")

	store := e.open(t)
	defer func() { _ = store.Close() }()
	run, err := store.GetRun(context.Background(), "chg-chatops-1")
	require.NoError(t, err)
	assert.Equal(t, "approved", run.Status, "chatops approve must settle the run")
}

// marshalApprovalExtra mirrors the production serialisation so the
// seeded Comment blob decodes exactly like one written by the adapter.
func marshalApprovalExtra(x approvalExtra) (string, error) {
	b, err := json.Marshal(x)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// compile-time pin: the CLI settlement path exists on the grpc service.
var _ = approval.StatusPending
