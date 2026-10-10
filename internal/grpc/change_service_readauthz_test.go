// change_service_readauthz_test.go covers the authorisation surface that #19
// left open: `plan` (a write that was never gated, so any authenticated caller
// could re-plan an approved change and reset it to draft), the 25 read RPCs
// (ActionView), the list counterpart of that gate (narrow the view, do not
// refuse the page), and the bulk grant list's identity source.
//
// The read posture is a decision, not an accident: an unattributable caller
// (shared --token: there is no principal to bind a decision to) passes the read
// gate, because denying such a caller filters nobody and only darkens
// dashboards. These tests pin BOTH halves — the denial for a real subject and
// the admission for a nameless one — so the trade cannot be reversed silently.
package grpc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexus/levee/internal/authz"
	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/pause"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// readPolicyMatrix: devcrew may act in dev and sees NOTHING in prod (not even
// view), so bob is the subject that proves filtering and denial; alice's team
// may view prod but may not plan there (prod grants only view, and her role
// supplies apply/rollback, never plan).
const (
	readMatrixYAML = `
teams:
  - name: sre
    environments:
      - name: dev
        actions: [plan, apply, approve, rollback, view]
      - name: prod
        actions: [view]
  - name: devcrew
    environments:
      - name: dev
        actions: [plan, apply, view]
`
	readRolesYAML = `
roles:
  - name: viewer
    permissions: [view]
  - name: operator
    parent: viewer
    permissions: [apply, rollback]
`
	readUsersYAML = `
users:
  - name: alice
    team: sre
    role: operator
  - name: bob
    team: devcrew
    role: operator
`
)

func newReadPolicyAuthorizer(t *testing.T) *authz.Authorizer {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.MatrixFileName), []byte(readMatrixYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, authz.RoleTreeFileName), []byte(readRolesYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "users.yaml"), []byte(readUsersYAML), 0o600))
	a, err := authz.Load(dir, "dev")
	require.NoError(t, err)
	require.True(t, a.Enforced())
	return a
}

// createIn makes a change in one environment and returns its id.
//
// The run is seeded straight into the store rather than through CreateChange.
// These tests are about plan/apply/read, and CreateChange is itself gated on
// `plan` in the declared environment (so a caller who may only VIEW prod could
// no longer create the prod fixture at all). Routing setup through the gate
// would entangle two different subjects; the create gate has its own test
// (TestCreateChange_RequiresPlanInTheDeclaredEnvironment). The fields mirrored
// here are the ones these tests read: environment, creator and a draft status.
func createIn(t *testing.T, svc *ChangeService, actor, env string) string {
	t.Helper()
	store, ok := any(svc.store).(state.Store)
	require.True(t, ok, "the service under test must expose a store")
	id := newID("run-")
	now := time.Now().UTC()
	require.NoError(t, store.CreateRun(context.Background(), &state.Run{
		ID:             id,
		Status:         "draft",
		ApprovalStatus: state.ApprovalStatusPending,
		ApprovalLevel:  "normal",
		Creator:        actor,
		IncidentID:     env,
		CreatedAt:      now,
		UpdatedAt:      now,
	}))
	return id
}

func policySvc(t *testing.T) (*ChangeService, state.Store) {
	t.Helper()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newReadPolicyAuthorizer(t))
	return svc, store
}

// --- plan is a write -------------------------------------------------------

func TestPlanChange_RequiresPlanPermission(t *testing.T) {
	ctx := context.Background()
	svc, store := policySvc(t)
	prodID := createIn(t, svc, "alice", "prod")

	// prod grants the team `view` only, and no role grants plan: planning
	// there is refused even though alice may apply an approved prod change.
	_, err := svc.PlanChange(ContextWithActor(ctx, "alice"),
		&pb.PlanChangeRequest{ChangeId: prodID, TargetHosts: []string{"h1"}})
	require.Error(t, err, "plan was never authorised before this change; a refusal here is the point")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), `action "plan"`)

	// Nothing may be persisted behind a refused plan.
	run, err := store.GetRun(ctx, prodID)
	require.NoError(t, err)
	assert.Empty(t, run.PlanJSON, "a denied plan must not persist an artifact")

	// dev grants plan, so the same caller succeeds there.
	devID := createIn(t, svc, "alice", "dev")
	_, err = svc.PlanChange(ContextWithActor(ctx, "alice"),
		&pb.PlanChangeRequest{ChangeId: devID, TargetHosts: []string{"h1"}})
	require.NoError(t, err, "a permitted plan must keep working")
}

func TestPlanChange_RefusesUnattributableCallerLikeOtherWrites(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	id := createIn(t, svc, "alice", "dev")

	_, err := svc.PlanChange(ctx, &pb.PlanChangeRequest{ChangeId: id, TargetHosts: []string{"h1"}})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err),
		"plan changes durable state, so it keeps the strict write posture")
}

// --- reads: ActionView ------------------------------------------------------

func TestReadRPCs_DenySubjectWithoutViewOnThatEnvironment(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	prodID := createIn(t, svc, "alice", "prod")
	bobCtx := ContextWithActor(ctx, "bob") // devcrew: no prod grant at all

	cases := []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := svc.GetChange(bobCtx, &pb.GetChangeRequest{Id: prodID}); return err }},
		{"logs", func() error { _, err := svc.GetLogs(bobCtx, &pb.GetLogsRequest{ChangeId: prodID}); return err }},
		{"trace", func() error { _, err := svc.GetTrace(bobCtx, &pb.GetTraceRequest{ChangeId: prodID}); return err }},
		{"diff", func() error { _, err := svc.GetDiff(bobCtx, &pb.GetDiffRequest{ChangeId: prodID}); return err }},
		// Streaming RPCs tail forever once they are admitted, so they are
		// called under a deadline: with the gate in place the refusal returns
		// before any tail loop, and if someone breaks the gate the test fails
		// loudly instead of eating the package timeout.
		{"watch", func() error {
			return bounded(t, func() error {
				return svc.WatchChange(&pb.WatchChangeRequest{ChangeId: prodID}, &fakeStream{ctx: bobCtx})
			})
		}},
		{"stream logs", func() error {
			return bounded(t, func() error {
				return svc.StreamLogs(&pb.StreamLogsRequest{ChangeId: prodID}, &fakeLogStream{ctx: bobCtx})
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err, "a caller with no view grant must not read this change's "+tc.name)
			assert.Equal(t, codes.PermissionDenied, status.Code(err))
			assert.Contains(t, err.Error(), `action "view"`)
			assert.Contains(t, err.Error(), "authz explain", "the refusal must be diagnosable")
		})
	}

	// The same change is readable to a subject whose team may view prod.
	got, err := svc.GetChange(ContextWithActor(ctx, "alice"), &pb.GetChangeRequest{Id: prodID})
	require.NoError(t, err)
	assert.Equal(t, prodID, got.GetId())
}

// TestReads_ByAnUnattributableCallerStayOpen pins the approved posture: the
// gate exists to separate teams that can be identified. A shared-token caller
// has no identity to judge, and denying reads to it removes the dashboards
// without removing anybody's access.
func TestReads_ByAnUnattributableCallerStayOpen(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	prodID := createIn(t, svc, "alice", "prod")

	// No subject in the context at all (shared --token, no X-Acting-As).
	got, err := svc.GetChange(ctx, &pb.GetChangeRequest{Id: prodID})
	require.NoError(t, err, "read filtering needs a subject; without one it must not silently deny")
	assert.Equal(t, prodID, got.GetId())

	_, err = svc.GetLogs(ctx, &pb.GetLogsRequest{ChangeId: prodID})
	require.NoError(t, err)

	// A write by the same caller is still refused — the asymmetry is the
	// decision, so both halves are asserted together.
	_, err = svc.ApplyChange(ctx, &pb.ApplyChangeRequest{ChangeId: prodID})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestListChanges_NarrowsInsteadOfRefusing(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	devID := createIn(t, svc, "alice", "dev")
	prodID := createIn(t, svc, "alice", "prod")

	resp, err := svc.ListChanges(ContextWithActor(ctx, "bob"), &pb.ListChangesRequest{})
	require.NoError(t, err, "a restricted list is not an error")
	ids := changeIDs(resp)
	assert.Contains(t, ids, devID, "bob's team may view dev")
	assert.NotContains(t, ids, prodID, "bob's team has no grant in prod; the row must not leak")

	// A caller with no viewable-environment restriction sees both.
	all, err := svc.ListChanges(ctx, &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.Len(t, changeIDs(all), 2, "an unattributable caller is not filtered")

	// A subject allowed everywhere keeps the whole list too (no over-filtering).
	everywhere, err := svc.ListChanges(ContextWithActor(ctx, "alice"), &pb.ListChangesRequest{})
	require.NoError(t, err)
	assert.Len(t, changeIDs(everywhere), 2,
		"alice's roles reach dev and her team may view prod, so nothing may drop")
}

func TestListChanges_FilteringCombinesWithStatusFilter(t *testing.T) {
	ctx := context.Background()
	svc, store := policySvc(t)
	devID := createIn(t, svc, "alice", "dev")
	prodID := createIn(t, svc, "alice", "prod")
	setRunStatus(t, store, devID, "running")
	setRunStatus(t, store, prodID, "running")

	resp, err := svc.ListChanges(ContextWithActor(ctx, "bob"),
		&pb.ListChangesRequest{Statuses: []string{"running", "failed"}})
	require.NoError(t, err)
	ids := changeIDs(resp)
	assert.Equal(t, []string{devID}, ids,
		"visibility must survive the client-side filter path, not just the fast path")
}

// --- bulk grant list: which identity decides ------------------------------

func TestBulkTransition_RefusesAssertedActorWithoutIdentity(t *testing.T) {
	ctx := context.Background()
	svc, store := bulkAuthFixture(t)

	auth := pause.NewSimplePermissionChecker(map[string][]string{
		"sre-oncall": {pause.PermissionPauseAll},
	})
	auth.SetDenyRecorder(pause.NewDenialAuditRecorder(store))
	svc.WithBulkPauseAuthorizer(auth)

	// actorKey alone is a client assertion (shared token mode): it must not
	// satisfy the grant list, and the caller must be told to authenticate.
	asserted := context.WithValue(ctx, actorKey{}, "sre-oncall")
	_, err := svc.PauseAll(asserted, &pb.PauseAllRequest{Reason: "drill"})
	require.Error(t, err, "a claimed name is not a proof of identity")
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Contains(t, err.Error(), "--auth-token")

	// The denial is audited even in that case (the checker saw it).
	denials, err := store.ListAudits(ctx, state.AuditFilter{Action: "permission.denied"})
	require.NoError(t, err)
	assert.NotZero(t, len(denials), "an unauthorised fleet-wide action must leave an audit trail")

	// Same request, this time with a provable subject: allowed.
	_, err = svc.PauseAll(ContextWithActor(ctx, "sre-oncall"), &pb.PauseAllRequest{Reason: "incident"})
	require.NoError(t, err)
}

func TestBulkTransition_StillRefusesKnownSubjectWithoutGrant(t *testing.T) {
	ctx := context.Background()
	svc, store := bulkAuthFixture(t)
	auth := pause.NewSimplePermissionChecker(map[string][]string{
		"alice": {pause.PermissionPauseAll},
	})
	auth.SetDenyRecorder(pause.NewDenialAuditRecorder(store))
	svc.WithBulkPauseAuthorizer(auth)

	_, err := svc.PauseAll(ContextWithActor(ctx, "mallory"), &pb.PauseAllRequest{Reason: "drill"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "not granted")
}

// --- GetConfig: the deployment decides, not the request --------------------

func configWithSecret(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.DataDir = t.TempDir()
	cfg.Database.Driver = "sqlite"
	cfg.Notify.Jira.APIToken = "jira-super-secret-token"
	return cfg
}

func TestGetConfig_OmittedRequestCannotLeakSecrets(t *testing.T) {
	cfg := configWithSecret(t)
	svc := NewSystemService(nil, cfg, "/etc/levee.yaml", "v", "c", "b", "go", time.Now())

	// The documented default: a caller that says nothing gets redaction. This
	// is the exact call shape that used to return raw credentials over gRPC,
	// because proto3 cannot distinguish "unset" from "false".
	resp, err := svc.GetConfig(context.Background(), &pb.GetConfigRequest{})
	require.NoError(t, err)
	assert.NotContains(t, string(resp.GetContent()), "jira-super-secret-token",
		"an omitted redact_secrets must not be read as a request for raw secrets")
	assert.Contains(t, string(resp.GetContent()), "REDACTED")

	// Asking for raw the old way (explicit false) is equally inert while the
	// deployment has not opted in.
	resp, err = svc.GetConfig(context.Background(), &pb.GetConfigRequest{RedactSecrets: false})
	require.NoError(t, err)
	assert.NotContains(t, string(resp.GetContent()), "jira-super-secret-token")

	// A nil request is the same case, not a licence.
	resp, err = svc.GetConfig(context.Background(), nil)
	require.NoError(t, err)
	assert.NotContains(t, string(resp.GetContent()), "jira-super-secret-token")
}

func TestGetConfig_RawRequiresTheDeploymentOptIn(t *testing.T) {
	cfg := configWithSecret(t)
	svc := NewSystemService(nil, cfg, "/etc/levee.yaml", "v", "c", "b", "go", time.Now())

	cfg.Security.ExposeRawConfig = true
	resp, err := svc.GetConfig(context.Background(), &pb.GetConfigRequest{RedactSecrets: false})
	require.NoError(t, err)
	assert.Contains(t, string(resp.GetContent()), "jira-super-secret-token",
		"with the switch on and the caller asking, raw is served — that is the opt-in's meaning")

	// Asking for redaction is always honoured, switch or no switch.
	resp, err = svc.GetConfig(context.Background(), &pb.GetConfigRequest{RedactSecrets: true})
	require.NoError(t, err)
	assert.NotContains(t, string(resp.GetContent()), "jira-super-secret-token")
}

// --- helpers ----------------------------------------------------------------

// bounded runs call and fails the test if it has not returned within 5s.
func bounded(t *testing.T, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("panic in streaming call: %v", r)
			}
		}()
		done <- call()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("streaming call did not return within 5s — an admitted read tails forever, " +
			"which means the authorisation gate never fired")
		return nil
	}
}

func changeIDs(resp *pb.ListChangesResponse) []string {
	var out []string
	for _, c := range resp.GetChanges() {
		out = append(out, c.GetId())
	}
	return out
}
