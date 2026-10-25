// approval_guard_wiring_test.go pins the CLI half of the approval decision
// guard: newApprovalService must build a vote-judging service when --as put
// a policy registry in effect, and the historical unjudged service when it
// did not. The guard itself is covered in internal/approval (the seam) and
// internal/grpc (the adapter); what only this package can prove is that the
// CLI's single construction point actually installs it.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/approval"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// votePolicyFixture: team sre has view only in dev — registered, reachable,
// and still not allowed to cast an approval vote.
const (
	voteMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [view]
`
	voteUsersYAML = `
users:
  - name: mallory
    team: sre
`
)

// newVoteEnv builds a temp data dir with the fixture policy and returns the
// config path to point optConfigPath at, plus the data dir (for approvals to
// live in the same registry --as loads).
func newVoteEnv(t *testing.T) (cfgPath, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "permissions.yaml"), []byte(voteMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "users.yaml"), []byte(voteUsersYAML), 0o600))
	cfgPath = filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("server:\n  data_dir: "+dataDir+"\n"), 0o600))
	return cfgPath, dataDir
}

// seedVoteFixture opens the store behind cfgPath and seeds one pending dev
// approval votable by voter. Returns the store and the approval id.
func seedVoteFixture(t *testing.T, cfgPath, voter string) (state.Store, string) {
	t.Helper()
	store, err := openStore(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID:             "run-vote",
		Status:         "draft",
		ApprovalStatus: state.ApprovalStatusPending,
		ApprovalLevel:  "normal",
		Creator:        "author",
		IncidentID:     "dev", // the env column, per grpc.envOf
		CreatedAt:      now,
		UpdatedAt:      now,
	}))

	seed := approval.NewService(newApprovalStoreAdapter(store))
	a, err := seed.Create(context.Background(), approval.CreateRequest{
		RunID:     "run-vote",
		Level:     approval.LevelStandard,
		Approvers: []string{voter},
	})
	require.NoError(t, err)
	return store, a.ID
}

// Under --as with a matrix that grants view only, a roster-listed voter is
// still refused by policy, and the refusal leaves the record pending.
func TestApprovalVoteUnderAsIsPolicyJudged(t *testing.T) {
	cfgPath, _ := newVoteEnv(t)
	prevConfig, prevAs := optConfigPath, optAs
	t.Cleanup(func() { optConfigPath, optAs = prevConfig, prevAs })
	optConfigPath, optAs = cfgPath, "mallory"

	store, approvalID := seedVoteFixture(t, cfgPath, "mallory")

	svc, err := newApprovalService(store)
	require.NoError(t, err)

	err = svc.Approve(context.Background(), approvalID, "mallory")
	require.ErrorIs(t, err, approval.ErrDecisionDenied)
	assert.Contains(t, err.Error(), "run-vote")

	still, getErr := store.GetApproval(context.Background(), approvalID)
	require.NoError(t, getErr)
	assert.Equal(t, "pending", still.Status, "a policy-refused vote must not touch the record")
}

// Without --as the very same vote succeeds: the roster alone judged it, the
// historical local posture. The two tests share one fixture on purpose —
// the only difference is the flag.
func TestApprovalVoteWithoutAsKeepsHistoricalPosture(t *testing.T) {
	cfgPath, _ := newVoteEnv(t)
	prevConfig, prevAs := optConfigPath, optAs
	t.Cleanup(func() { optConfigPath, optAs = prevConfig, prevAs })
	optConfigPath, optAs = cfgPath, "" // no --as: unjudged

	store, approvalID := seedVoteFixture(t, cfgPath, "mallory")

	svc, err := newApprovalService(store)
	require.NoError(t, err)
	require.NoError(t, svc.Approve(context.Background(), approvalID, "mallory"))
}
