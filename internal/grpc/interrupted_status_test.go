// interrupted_status_test.go pins the cluster-takeover terminal state's
// full vocabulary (B4): the state machine accepts it as an archive source
// and rejects cancelling it, RetryChange admits it as the re-drive entry
// point (design §7.5-Q1), and WatchChange treats it as terminal.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestInterrupted_Transitions(t *testing.T) {
	// interrupted may be archived (records hygiene) but never cancelled
	// (would rewrite the takeover verdict the audit chain recorded).
	assert.True(t, isValidTransition("interrupted", "archived"))
	assert.False(t, isValidTransition("interrupted", "cancelled"))
	assert.False(t, isValidTransition("interrupted", "running"))
	assert.False(t, isValidTransition("interrupted", "paused"))

	// Sanity of the surrounding vocabulary (guard against accidental
	// edits of neighbouring clauses).
	assert.True(t, isValidTransition("failed", "archived"))
	assert.True(t, isValidTransition("running", "cancelled"))
	assert.False(t, isValidTransition("completed", "cancelled"))
}

func TestRetryChange_InterruptedAdmitted(t *testing.T) {
	// Q1: interrupted is the machine entry point for re-driving a
	// takeover-settled change. The engine-less fallback path transitions
	// to draft (mirroring the failed/rolled_back fallback), which is
	// enough to prove admission here; the full re-execution path is
	// covered by the wiring/integration suites.
	svc, store := newTestChangeServiceWithEngine(t, nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "retry-interrupted"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "interrupted")

	resp, err := svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: created.GetId()})
	require.NoError(t, err, "interrupted changes must be retry-admitted")
	assert.Equal(t, "draft", resp.GetStatus())
}

func TestRetryChange_StillRefusesOtherStates(t *testing.T) {
	svc, store := newTestChangeServiceWithEngine(t, nil)
	created, err := svc.CreateChange(context.Background(), &pb.CreateChangeRequest{Label: "retry-gate"})
	require.NoError(t, err)
	setRunStatus(t, store, created.GetId(), "completed")

	_, err = svc.RetryChange(context.Background(), &pb.RetryRequest{ChangeId: created.GetId()})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, err.Error(), "interrupted", "the refusal message must name the full retryable set")
}
