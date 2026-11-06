// change_service_runauthz_test.go pins the rule these three RPCs broke in
// the same way: the request can name BOTH a change and a run, the data is
// read by whichever run id wins, and the gate judged only the change.
//
// GetLogs / StreamLogs / GetTrace all resolved `runID = changeID; if
// run_id != "" { runID = run_id }` and then authorized change_id — so a
// caller entitled to read one change could read another environment's step
// output (Stdout/Stderr: command text and credential echoes) or audit
// trace by swapping the run id. GetDiff echoes the caller's run filter and
// is gated on it for the same reason: an echoed id is an answer about that
// id. The rule encoded here: every id a request names is judged, and the
// env a read is judged in is the RUN's env (IncidentID — see envOf).

package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

// seedLogStep plants a step row with distinctive output under a run. The
// marker string is the payload the old (change-only) gate would have
// leaked, so a regression reads as a failed assertion, not a silent pass.
func seedLogStep(t *testing.T, store state.Store, runID, marker string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	batchID := newID("bat-")
	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID:         batchID,
		RunID:      runID,
		BatchNo:    1,
		Status:     "success",
		TotalHosts: 1,
		Succeeded:  1,
		StartedAt:  &now,
	}))
	require.NoError(t, store.CreateStep(ctx, &state.Step{
		ID:          newID("stp-"),
		RunID:       runID,
		BatchID:     batchID,
		Host:        "h1",
		StepName:    "step",
		Action:      "cmd",
		Status:      state.StepStatusSuccess,
		Stdout:      marker,
		StartedAt:   &now,
		CompletedAt: &now,
	}))
}

// crossEnvRequestor is bob of the read-policy fixture: devcrew grants him
// view in dev and NOTHING in prod.
func TestGetLogs_JudgesTheRunIdItReadsBy(t *testing.T) {
	ctx := context.Background()
	svc, store := policySvc(t)
	devID := createIn(t, svc, "bob", "dev")
	prodID := createIn(t, svc, "alice", "prod")
	seedLogStep(t, store, prodID, "PROD-STEP-SECRET")

	// The change he may read, paired with a run he may not.
	_, err := svc.GetLogs(ContextWithActor(ctx, "bob"),
		&pb.GetLogsRequest{ChangeId: devID, RunId: prodID})
	require.Error(t, err, "judging only the change would replay prod step output to a dev-only caller")
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// The pairing he IS entitled to still works.
	resp, err := svc.GetLogs(ContextWithActor(ctx, "bob"),
		&pb.GetLogsRequest{ChangeId: devID, RunId: devID})
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestGetTrace_JudgesTheRunIdItReadsBy(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	devID := createIn(t, svc, "bob", "dev")
	prodID := createIn(t, svc, "alice", "prod")

	_, err := svc.GetTrace(ContextWithActor(ctx, "bob"),
		&pb.GetTraceRequest{ChangeId: devID, RunId: prodID})
	require.Error(t, err, "the audit trail of a forbidden env must not be walked through a readable change")
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestGetDiff_JudgesTheEchoedRunFilter(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	devID := createIn(t, svc, "bob", "dev")
	prodID := createIn(t, svc, "alice", "prod")

	_, err := svc.GetDiff(ContextWithActor(ctx, "bob"),
		&pb.GetDiffRequest{ChangeId: devID, RunId: prodID})
	require.Error(t, err, "the response echoes the requested run filter; a forbidden id earns no echo")
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}
