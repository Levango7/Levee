// change_service_bulkauth_test.go pins P0-1's first live authorization
// decision: the fleet-wide pause-all / resume-all actions must honour the
// same grant list the CLI honours. Before this, `levee pause all` checked
// permissions while the equivalent RPC mutated every running change with no
// authorisation at all — so an operator's configured permission set
// changed nothing over the wire.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/pause"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// bulkAuthFixture returns a service with one running change to act on.
func bulkAuthFixture(t *testing.T) (*ChangeService, state.Store) {
	t.Helper()
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	created, err := svc.CreateChange(ContextWithActor(ctx, "operator"),
		&pb.CreateChangeRequest{Label: "bulk-target"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "running")
	return svc, store
}

func TestBulkTransitionWithoutAuthorizerStaysOpen(t *testing.T) {
	ctx := context.Background()
	svc, _ := bulkAuthFixture(t)

	// Deliberate posture (see WithBulkPauseAuthorizer): an absent grant
	// list is not treated as an empty one, because bulk pause is the
	// mitigation operators reach for mid-incident. The server announces
	// this posture at startup rather than deciding silently per request.
	resp, err := svc.PauseAll(ctx, &pb.PauseAllRequest{Reason: "drill"})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.GetPausedChangeIds())
}

func TestBulkTransitionEnforcesGrantList(t *testing.T) {
	ctx := context.Background()
	svc, store := bulkAuthFixture(t)

	auth := pause.NewSimplePermissionChecker(map[string][]string{
		"alice": {pause.PermissionPauseAll},
	})
	auth.SetDenyRecorder(pause.NewDenialAuditRecorder(store))
	svc.WithBulkPauseAuthorizer(auth)

	// The granted actor gets through.
	ok, err := svc.PauseAll(ContextWithActor(ctx, "alice"), &pb.PauseAllRequest{Reason: "incident"})
	require.NoError(t, err)
	assert.NotEmpty(t, ok.GetPausedChangeIds())

	// An ungranted actor is refused. Nothing may have been touched: the
	// decision sits before the run scan, so a refusal cannot half-pause
	// the fleet.
	_, err = svc.PauseAll(ContextWithActor(ctx, "bob"), &pb.PauseAllRequest{Reason: "snooze"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), pause.PermissionPauseAll)

	// A pause grant is not a resume grant.
	_, err = svc.ResumeAll(ContextWithActor(ctx, "alice"), &pb.PauseAllRequest{Reason: "go"})
	require.Error(t, err, "pause:all must not authorise resume-all")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), pause.PermissionResumeAll)

	// The denial is auditable (SA-007) — the only reason a default-open
	// posture is defensible at all.
	// ListAudits is timestamp DESC, so assert on the set rather than a
	// positional index.
	denials, err := store.ListAudits(ctx, state.AuditFilter{Action: pause.ActionPermissionDenied})
	require.NoError(t, err)
	require.Len(t, denials, 2, "both refusals must leave an audit trail")
	seen := map[string]string{}
	for _, d := range denials {
		seen[d.Actor] = d.Target
	}
	assert.Equal(t, map[string]string{
		"bob":   pause.PermissionPauseAll,
		"alice": pause.PermissionResumeAll,
	}, seen, "each denial records its own actor and the permission that was missing")
}

// TestBulkTransitionOverNamedTokenIgnoresForgedActor pins that the grant is
// keyed on the acting identity, and that with a named credential that
// identity is the token's subject — x-actor cannot borrow a wider grant.
func TestBulkTransitionOverNamedTokenIgnoresForgedActor(t *testing.T) {
	srv, store := startTestServerWithAllServices(t,
		WithAuthTokens([]TokenIdentity{{Token: "alice-token", Subject: "alice"}}))
	client := pb.NewChangeServiceClient(newInsecureClient(t, srv.Addr()))

	// One metadata blob: NewOutgoingContext replaces rather than merges,
	// so layering the two would silently drop the bearer credential.
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs(
		"authorization", "Bearer alice-token",
		"x-actor", "root",
	))

	// One running change, so the action has something to act on and
	// records a per-run audit entry.
	created, err := client.CreateChange(ctx, &pb.CreateChangeRequest{Label: "wire-target"})
	require.NoError(t, err)
	seed, err := store.GetRun(context.Background(), created.GetId())
	require.NoError(t, err)
	seed.Status = "running"
	require.NoError(t, store.UpdateRun(context.Background(), seed))

	_, err = client.PauseAll(ctx, &pb.PauseAllRequest{Reason: "claim to be root"})
	// No authorizer is wired on this server, so the call is allowed; the
	// point is therefore about attribution, not admission.
	require.NoError(t, err)

	audits, aerr := store.ListAudits(context.Background(), state.AuditFilter{Action: "pause-all"})
	require.NoError(t, aerr)
	require.NotEmpty(t, audits)
	assert.Equal(t, "alice", audits[0].Actor,
		"the audit must record the authenticated subject, not the forged x-actor")
}
