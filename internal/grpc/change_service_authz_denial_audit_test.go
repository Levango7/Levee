package grpc

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// devViewerPolicy: dave is registered and may look at dev only. Prod is
// reachable (so the gate judges it rather than falling off the map) but
// nothing grants it — the shape a refusal needs to be a refusal.
const (
	devViewerMatrixYAML = `
teams:
  - name: ops
    environments:
      - name: dev
        actions: [view]
`
	devViewerUsersYAML = `
users:
  - name: dave
    team: ops
`
)

func newDevViewerAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(devViewerMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(devViewerUsersYAML), 0o600))
	a, err := authz.Load(dir, "")
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

func denialRows(t *testing.T, store state.Store) []*state.Audit {
	t.Helper()
	rows, err := store.ListAudits(context.Background(), state.AuditFilter{Action: audit.ActionPermissionDenied})
	require.NoError(t, err)
	return rows
}

// TestDeniedApplyIsRecordedInTheAuditChain is SA-007's contact point on the
// serving path: before this, the refusal existed only in the gRPC status the
// caller received — the caller could drop it, and nothing anywhere recorded
// that LEVEE said no.
func TestDeniedApplyIsRecordedInTheAuditChain(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	// carol is reachable in prod but neither axis grants apply (see
	// policyMatrixYAML in change_service_authz_test.go).
	svc.WithAuthorizer(newPolicyAuthorizer(t).WithDenialRecorder(audit.NewDenialRecorder(store)))
	id := approvedProdRun(t, svc, store)

	_, err := svc.ApplyChange(ContextWithActor(ctx, "carol"), &pb.ApplyChangeRequest{ChangeId: id})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	rows := denialRows(t, store)
	require.Len(t, rows, 1, "exactly one refusal happened, so exactly one row may be written")
	assert.Equal(t, audit.ResultDenied, rows[0].Result)
	assert.Equal(t, "carol", rows[0].Actor)
	assert.Equal(t, "apply@prod", rows[0].Target, "the refused permission and the judged environment")
	assert.Empty(t, rows[0].RunID, "audit.run_id carries no foreign key precisely so a refusal can be recorded run-lessly")
}

// TestDeniedReadIsRecordedButListFilteringIsNot draws the boundary: a request
// that is turned away is one audit row; a list that hides rows is none. The
// filter answers "may this caller see this environment" per row, and recording
// there would write one denial per hidden row — a 200-row page would bury the
// chain in 200 rows for one question.
func TestDeniedReadIsRecordedButListFilteringIsNot(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	// Created before the authorizer is installed: dave may not write at all,
	// and this test is about the read gate.
	id := approvedProdRun(t, svc, store)
	a := newDevViewerAuthorizer(t).WithDenialRecorder(audit.NewDenialRecorder(store))
	svc.WithAuthorizer(a)

	_, err := svc.GetChange(ContextWithActor(ctx, "dave"), &pb.GetChangeRequest{Id: id})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	rows := denialRows(t, store)
	require.Len(t, rows, 1)
	assert.Equal(t, "view@prod", rows[0].Target)
	assert.Equal(t, "dave", rows[0].Actor)

	visible := resourceVisibility(ContextWithActor(ctx, "dave"), a)
	require.NotNil(t, visible, "dave cannot see every environment, so filtering must be active")
	assert.False(t, visible("prod"))
	assert.False(t, visible("prod"))
	assert.True(t, visible("dev"))

	assert.Len(t, denialRows(t, store), 1,
		"filtering is not refusing: three predicate calls for one question must not add rows")
}
