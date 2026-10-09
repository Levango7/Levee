package authz

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/permission"
)

// devOnlyMatrixYAML grants `sre` dev and says nothing about prod, so one
// subject is one environment away from a refusal in each direction.
const devOnlyMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, view]
`

const devOnlyUsersYAML = `
users:
  - name: alice
    team: sre
`

func newDevOnlyAuthorizer(t *testing.T) *Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, MatrixFileName), []byte(devOnlyMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(devOnlyUsersYAML), 0o600))
	a, err := Load(dir, "dev")
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

func TestRecordDenial_ReportsTheQuestionThatWasJudged(t *testing.T) {
	a := newDevOnlyAuthorizer(t)
	type call struct{ actor, action, env string }
	var got []call
	a.WithDenialRecorder(func(_ context.Context, actor, action, env string) {
		got = append(got, call{actor, action, env})
	})

	d := a.Decide("alice", "prod", permission.ActionApply)
	require.False(t, d.Allowed)
	a.RecordDenial(context.Background(), d)

	require.Len(t, got, 1)
	assert.Equal(t, call{"alice", permission.ActionApply, "prod"}, got[0])
}

func TestRecordDenial_IsSilentOnAllowAndWithoutARecorder(t *testing.T) {
	a := newDevOnlyAuthorizer(t)
	fired := 0
	a.WithDenialRecorder(func(context.Context, string, string, string) { fired++ })

	allowed := a.Decide("alice", "dev", permission.ActionApply)
	require.True(t, allowed.Allowed)
	a.RecordDenial(context.Background(), allowed)
	assert.Zero(t, fired, "an allow is not a refusal")

	// A cleared recorder must leave RecordDenial inert rather than panic: the
	// recorder is an observability seam, and losing it cannot be allowed to
	// take down the refusal path itself.
	a.WithDenialRecorder(nil)
	denied := a.Decide("alice", "prod", permission.ActionApply)
	require.False(t, denied.Allowed)
	require.NotPanics(t, func() { a.RecordDenial(context.Background(), denied) })
	assert.Zero(t, fired)
}

func TestRecordDenial_NamesTheUnattributableCaller(t *testing.T) {
	a := newDevOnlyAuthorizer(t)
	var actor, env string
	a.WithDenialRecorder(func(_ context.Context, who, _, where string) { actor, env = who, where })

	// No subject at all: the write gate refuses this caller outright, and the
	// row has to say so rather than looking like a system action.
	d := a.Decide("", "prod", permission.ActionApply)
	require.False(t, d.Allowed)
	a.RecordDenial(context.Background(), d)

	assert.Equal(t, UnattributableActor, actor)
	assert.Equal(t, "prod", env)
}

func TestRecordDenial_NilReceiverIsInert(t *testing.T) {
	var a *Authorizer
	require.NotPanics(t, func() {
		// Also exercises the builder returning the receiver unchanged.
		assert.Nil(t, a.WithDenialRecorder(func(context.Context, string, string, string) {}))
		a.RecordDenial(context.Background(), Decision{Subject: "alice", Action: "apply"})
	})
}
