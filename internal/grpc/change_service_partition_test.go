// change_service_partition_test.go covers the disconnect policy wired through
// WithPartitionChecker: a partitioned serve must refuse new apply and new
// approval with a readable reason, before it spends a database round trip
// that would fail deeper with a less actionable error. The tests also pin the
// absence of the policy (nil checker) so a deployment that never wired one
// keeps the historical behaviour.
package grpc

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/partition"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failingPinger stands in for a database that stopped answering.
type failingPinger struct{ err error }

func (f failingPinger) Ping(context.Context) error { return f.err }

// countingStore counts GetRun calls so a test can prove the guard fired
// before the first read rather than after it.
type countingStore struct {
	state.Store
	getRunCalls int
}

func (c *countingStore) GetRun(ctx context.Context, id string) (*state.Run, error) {
	c.getRunCalls++
	return c.Store.GetRun(ctx, id)
}

func TestApplyChange_PartitionGuardRefusesBeforeReading(t *testing.T) {
	base := newTestStore(t)
	store := &countingStore{Store: base}
	svc := NewChangeService(store, nil, nil, nil).
		WithPartitionChecker(partition.NewChecker(
			failingPinger{err: errors.New("connection refused")}, 0))

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "anything"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	msg := status.Convert(err).Message()
	assert.Contains(t, msg, "control plane database unreachable")
	// Naming the policy is what turns a refusal into an operator action.
	assert.Contains(t, msg, "design-disconnect-policy.md")
	assert.Equal(t, 0, store.getRunCalls,
		"the guard must fire before the first store read — a doomed read is both wasted and less honest")
}

func TestApproveChange_PartitionGuardRefuses(t *testing.T) {
	store := &countingStore{Store: newTestStore(t)}
	svc := NewChangeService(store, nil, nil, nil).
		WithPartitionChecker(partition.NewChecker(
			failingPinger{err: errors.New("i/o timeout")}, 0))

	_, err := svc.ApproveChange(context.Background(), &pb.ApproveRequest{ChangeId: "anything"})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Contains(t, status.Convert(err).Message(), "control plane database unreachable")
	assert.Equal(t, 0, store.getRunCalls)
}

// A reachable database must not change the outcome: the request proceeds past
// the guard and fails later for its own reason (here, an unknown change).
// This is the test that would catch a guard wired as "always refuse".
func TestPartitionGuard_ReachableDatabaseDoesNotRefuse(t *testing.T) {
	svc, _ := newTestChangeService(t)
	svc.WithPartitionChecker(partition.NewChecker(failingPinger{}, 0))

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "missing"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err),
		"a reachable database must leave the historical verdict untouched")
}

// The nil checker is the state of every deployment that never wired the
// policy — single-node, tests, reduced-functionality servers. It must not
// invent a refusal they did not ask for.
func TestPartitionGuard_NilCheckerKeepsHistoricalBehaviour(t *testing.T) {
	svc, _ := newTestChangeService(t)

	_, err := svc.ApplyChange(context.Background(), &pb.ApplyChangeRequest{ChangeId: "missing"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// The policy guards the two entries that lead to execution. A read path must
// stay answerable while partitioned (it degrades honestly on its own), so
// GetChange is deliberately not behind the guard — this pins that boundary.
func TestPartitionGuard_DoesNotCoverReadPaths(t *testing.T) {
	svc, _ := newTestChangeService(t)
	svc.WithPartitionChecker(partition.NewChecker(
		failingPinger{err: errors.New("connection refused")}, 0))

	_, err := svc.GetChange(context.Background(), &pb.GetChangeRequest{Id: "missing"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err),
		"reads must not be refused by the disconnect policy")
}
