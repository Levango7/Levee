// change_service_write_authz_test.go pins the authorization of the write RPCs
// that had none: CreateChange, CloneChange, RetryChange, RetryHost,
// CancelChange, PauseChange, ResumeChange and ArchiveChange. Before this batch
// a caller who could not even VIEW an environment could create a change in it,
// re-drive a failed one, stop a running one, or archive — and purge the
// evidence of — a finished one.
//
// What matters per RPC is not that a gate exists but three properties:
//
//  1. a subject the policy refuses gets PermissionDenied (never a different
//     status that would leak where the refusal happened);
//  2. the refusal precedes every effect — no status write, no engine call, no
//     audit row for the operation, no artifact purge;
//  3. the audited environment is the one JUDGED (the declared env for create
//     and clone, the run's own env for the rest), because judging the source
//     of a clone or the request's env for a retry would let a dev-granted
//     caller act on prod.
//
// The fixtures seed runs straight into the store: these tests are about the
// write gates, and the create gate would otherwise refuse to build the fixture
// for prod before the subject under test is reached (that gate has its own test
// below).
package grpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/audit"
	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// runCount counts the runs in the store, for "a denied create created nothing".
func runCount(t *testing.T, store state.Store) int {
	t.Helper()
	runs, err := store.ListRuns(context.Background(), state.RunFilter{})
	require.NoError(t, err)
	return len(runs)
}

// auditActionCount counts audit rows for one action, for "a refused operation
// left no trace of itself" (the denial row is a different action by design).
func auditActionCount(t *testing.T, store state.Store, action string) int {
	t.Helper()
	rows, err := store.ListAudits(context.Background(), state.AuditFilter{Action: action})
	require.NoError(t, err)
	return len(rows)
}

func TestCreateChange_RequiresPlanInTheDeclaredEnvironment(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	before := runCount(t, store)

	// prod grants the team view only, and no role grants plan there.
	_, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "nope", Environment: "prod"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), `action "plan"`)
	assert.Equal(t, before, runCount(t, store), "a denied create must not persist a run")

	// dev grants plan, so the same caller proceeds there.
	created, err := svc.CreateChange(ContextWithActor(ctx, "alice"),
		&pb.CreateChangeRequest{Label: "ok", Environment: "dev"})
	require.NoError(t, err)
	assert.Equal(t, "dev", created.GetEnvironment())
	assert.Equal(t, before+1, runCount(t, store))

	// An unidentifiable caller cannot be judged: the refusal must say so.
	_, err = svc.CreateChange(ctx, &pb.CreateChangeRequest{Label: "anon", Environment: "dev"})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Equal(t, before+1, runCount(t, store))
}

func TestCloneChange_JudgesTheEnvironmentTheNewChangeLandsIn(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	before := runCount(t, store)

	devSource := seedRunIn(t, store, "alice", "dev")
	prodSource := seedRunIn(t, store, "alice", "prod")

	// The source's environment decides when no override is given.
	_, err := svc.CloneChange(ContextWithActor(ctx, "alice"),
		&pb.CloneChangeRequest{SourceChangeId: prodSource, Label: "cloned"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, before+2, runCount(t, store), "a denied clone must not persist a run")

	// An override into a forbidden environment is refused too: the clone is a
	// change IN prod even though its source lives in dev. Judging the source
	// alone is the hole this asserts against.
	_, err = svc.CloneChange(ContextWithActor(ctx, "alice"),
		&pb.CloneChangeRequest{SourceChangeId: devSource, Label: "cloned", Environment: "prod"})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, before+2, runCount(t, store))

	// The same clone without the override is allowed (dev grants plan).
	cloned, err := svc.CloneChange(ContextWithActor(ctx, "alice"),
		&pb.CloneChangeRequest{SourceChangeId: devSource, Label: "cloned"})
	require.NoError(t, err)
	assert.Equal(t, "dev", cloned.GetEnvironment())
	assert.Equal(t, before+3, runCount(t, store))
}

func TestRetryChange_RequiresApplyInTheRunsEnvironment(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{runID: "exec-retry", runSuccess: true, runPhase: "completed"}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, id, "failed")
	beforeAudit := auditActionCount(t, store, state.AuditActionRetry)

	// carol may not apply anywhere: her role stops at view.
	_, err := svc.RetryChange(ContextWithActor(ctx, "carol"), &pb.RetryRequest{ChangeId: id})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, "failed", statusOf(t, store, id), "a denied retry must not move the run")
	assert.Equal(t, beforeAudit, auditActionCount(t, store, state.AuditActionRetry),
		"a denied retry must not leave an audit row claiming it happened")

	// alice's role supplies apply in prod, so the same call proceeds.
	_, err = svc.RetryChange(ContextWithActor(ctx, "alice"), &pb.RetryRequest{ChangeId: id})
	require.NoError(t, err)
	assert.Equal(t, beforeAudit+1, auditActionCount(t, store, state.AuditActionRetry))
}

func TestRetryHost_RequiresApplyAndRefusesBeforeTheBudget(t *testing.T) {
	ctx := context.Background()
	rec := &recordingEngine{runID: "exec-host", runSuccess: true, runPhase: "completed"}
	svc, store := newTestChangeServiceWithEngine(t, rec.adapter())
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, id, "failed")

	// The host list is valid and the budget is untouched, so the ONLY reason
	// this can fail is the permission gate — and it must fail as a permission
	// refusal, not as "host list required" or "budget exhausted".
	_, err := svc.RetryHost(ContextWithActor(ctx, "carol"),
		&pb.RetryHostRequest{ChangeId: id, Hosts: []string{"web-1"}})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), "retry-host denied")
	assert.Equal(t, "failed", statusOf(t, store, id))
}

func TestCancelChange_RequiresCancelAndLeavesTheRunUntouched(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, id, "running")
	beforeAudit := auditActionCount(t, store, state.AuditActionCancel)

	// Nobody in this fixture holds `cancel` in prod; even `force` must not
	// bypass the gate (it relaxes the state machine, not the policy).
	_, err := svc.CancelChange(ContextWithActor(ctx, "alice"),
		&pb.CancelRequest{ChangeId: id, Reason: "denied", Force: true})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, "running", statusOf(t, store, id), "a denied cancel must not stop the run")
	assert.Equal(t, beforeAudit, auditActionCount(t, store, state.AuditActionCancel))
}

func TestPauseResume_RequireTheirOwnAction(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	running := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, running, "running")
	paused := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, paused, "paused")

	_, err := svc.PauseChange(ContextWithActor(ctx, "alice"), &pb.PauseRequest{ChangeId: running})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), `action "pause"`)
	assert.Equal(t, "running", statusOf(t, store, running))

	_, err = svc.ResumeChange(ContextWithActor(ctx, "alice"), &pb.PauseRequest{ChangeId: paused})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), `action "resume"`)
	assert.Equal(t, "paused", statusOf(t, store, paused))
}

func TestArchiveChange_RequiresAdminAndNeverPurgesAnUnauthorisedChange(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	svc.WithAuthorizer(newPolicyAuthorizer(t))
	id := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, id, "completed")
	beforeAudit := auditActionCount(t, store, state.AuditActionArchive)

	// alice may apply an approved prod change but archiving is records
	// management (and purge_artifacts deletes evidence), so it takes admin.
	_, err := svc.ArchiveChange(ContextWithActor(ctx, "alice"),
		&pb.ArchiveRequest{ChangeId: id, PurgeArtifacts: true})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Contains(t, err.Error(), `action "admin"`)
	assert.Equal(t, "completed", statusOf(t, store, id), "a denied archive must not file the change away")
	assert.Equal(t, beforeAudit, auditActionCount(t, store, state.AuditActionArchive))
}

// TestWriteGatesRecordTheirDenials: SA-007's contact point for this batch. The
// audit row is what makes a refusal reviewable later; the caller's error
// message is not.
func TestWriteGatesRecordTheirDenials(t *testing.T) {
	ctx := context.Background()
	svc, store := newTestChangeService(t)
	// The recorder is attached the way serve wires it (WithDenialRecorder);
	// without it the authorizer refuses but nothing is written down.
	svc.WithAuthorizer(newPolicyAuthorizer(t).WithDenialRecorder(audit.NewDenialRecorder(store)))
	id := seedRunIn(t, store, "alice", "prod")
	setRunStatus(t, store, id, "running")
	before := len(denialRows(t, store))

	_, err := svc.CancelChange(ContextWithActor(ctx, "carol"), &pb.CancelRequest{ChangeId: id})
	require.Error(t, err)

	rows := denialRows(t, store)
	require.Len(t, rows, before+1, "the refused cancel must be recorded as a denial")
	// The target is the owner's own shape (action@env), asserted through its
	// helper rather than a literal so a change there cannot pass unnoticed.
	assert.Equal(t, audit.DenialTarget(state.AuditActionCancel, "prod"), rows[len(rows)-1].Target,
		"the denial row names the refused action and the environment that was judged")
	assert.Equal(t, "carol", rows[len(rows)-1].Actor)
}

// statusOf reads a run's status back.
func statusOf(t *testing.T, store state.Store, id string) string {
	t.Helper()
	run, err := store.GetRun(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, run)
	return run.Status
}
