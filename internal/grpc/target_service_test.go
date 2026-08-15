// target_service_test.go tests the TargetService gRPC handler. Targets are
// held in an in-memory registry, so no store is required. CheckTarget is
// tested without a ChannelFactory (cached path) to keep tests hermetic.
package grpc

import (
	"context"
	"testing"

	"github.com/nexus/levee/internal/grpc/pb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// newTestTargetService returns a TargetService with an empty in-memory
// registry and no channel factory.
func newTestTargetService(t *testing.T) *TargetService {
	t.Helper()
	return NewTargetService(nil)
}

// =========================================================================
// AddTarget
// =========================================================================

func TestAddTarget_Success(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	resp, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Hostname:    "web-01.example.com",
		ChannelType: "ssh",
		Port:        22,
		Labels:      map[string]string{"env": "prod", "role": "web"},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)

	assert.NotEmpty(t, resp.GetId())
	assert.Equal(t, "web-01.example.com", resp.GetHostname())
	assert.Equal(t, "ssh", resp.GetChannelType())
	assert.Equal(t, int32(22), resp.GetPort())
	assert.Equal(t, "prod", resp.GetLabels()["env"])
}

func TestAddTarget_DefaultPort(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	// SSH default port.
	resp, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Hostname:    "host1",
		ChannelType: "ssh",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(22), resp.GetPort())

	// WinRM default port.
	resp2, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Hostname:    "host2",
		ChannelType: "winrm",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(5985), resp2.GetPort())
}

func TestAddTarget_ExplicitID(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	resp, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id:       "my-target-1",
		Hostname: "host1",
	})
	require.NoError(t, err)
	assert.Equal(t, "my-target-1", resp.GetId())
}

func TestAddTarget_DuplicateID(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id:       "tgt-1",
		Hostname: "host1",
	})
	require.NoError(t, err)

	_, err = svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id:       "tgt-1",
		Hostname: "host2",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.AlreadyExists, st.Code())
}

func TestAddTarget_MissingHostname(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		ChannelType: "ssh",
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

func TestAddTarget_NilRequest(t *testing.T) {
	svc := newTestTargetService(t)
	_, err := svc.AddTarget(context.Background(), nil)
	require.Error(t, err)
}

// =========================================================================
// RemoveTarget
// =========================================================================

func TestRemoveTarget_Success(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: "tgt-1", Hostname: "host1"})
	require.NoError(t, err)

	_, err = svc.RemoveTarget(ctx, &pb.RemoveTargetRequest{Id: "tgt-1"})
	require.NoError(t, err)

	// Verify it's gone.
	_, err = svc.GetTarget(ctx, &pb.GetTargetRequest{Id: "tgt-1"})
	require.Error(t, err)
}

func TestRemoveTarget_NotFound(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.RemoveTarget(ctx, &pb.RemoveTargetRequest{Id: "nonexistent"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestRemoveTarget_EmptyID(t *testing.T) {
	svc := newTestTargetService(t)
	_, err := svc.RemoveTarget(context.Background(), &pb.RemoveTargetRequest{Id: ""})
	require.Error(t, err)
}

// =========================================================================
// GetTarget
// =========================================================================

func TestGetTarget_Success(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id:       "tgt-1",
		Hostname: "web-01",
		Labels:   map[string]string{"env": "staging"},
	})
	require.NoError(t, err)

	resp, err := svc.GetTarget(ctx, &pb.GetTargetRequest{Id: "tgt-1"})
	require.NoError(t, err)
	assert.Equal(t, "web-01", resp.GetHostname())
	assert.Equal(t, "staging", resp.GetLabels()["env"])
}

func TestGetTarget_NotFound(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.GetTarget(ctx, &pb.GetTargetRequest{Id: "nope"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

// =========================================================================
// ListTargets
// =========================================================================

func TestListTargets_Empty(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	resp, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetTargets())
	assert.Equal(t, int32(0), resp.GetTotalSize())
}

func TestListTargets_All(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	for _, id := range []string{"tgt-a", "tgt-b", "tgt-c"} {
		_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: id, Hostname: id})
		require.NoError(t, err)
	}

	resp, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{})
	require.NoError(t, err)
	assert.Equal(t, int32(3), resp.GetTotalSize())
	assert.Len(t, resp.GetTargets(), 3)
}

func TestListTargets_FilterByChannelType(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t1", Hostname: "h1", ChannelType: "ssh"})
	require.NoError(t, err)
	_, err = svc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t2", Hostname: "h2", ChannelType: "winrm"})
	require.NoError(t, err)

	resp, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{ChannelType: "ssh"})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.GetTotalSize())
	assert.Equal(t, "ssh", resp.GetTargets()[0].GetChannelType())
}

func TestListTargets_FilterByLabels(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id: "t1", Hostname: "h1", Labels: map[string]string{"env": "prod"},
	})
	require.NoError(t, err)
	_, err = svc.AddTarget(ctx, &pb.AddTargetRequest{
		Id: "t2", Hostname: "h2", Labels: map[string]string{"env": "dev"},
	})
	require.NoError(t, err)

	resp, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{
		LabelSelector: map[string]string{"env": "prod"},
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), resp.GetTotalSize())
	assert.Equal(t, "t1", resp.GetTargets()[0].GetId())
}

func TestListTargets_Pagination(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		id := string(rune('a'+i)) + "-id"
		_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: id, Hostname: id})
		require.NoError(t, err)
	}

	// Page 1: size 2.
	resp, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{PageSize: 2})
	require.NoError(t, err)
	assert.Len(t, resp.GetTargets(), 2)
	assert.Equal(t, int32(5), resp.GetTotalSize())
	assert.NotEmpty(t, resp.GetNextPageToken())

	// Page 2.
	resp2, err := svc.ListTargets(ctx, &pb.ListTargetsRequest{PageSize: 2, PageToken: resp.GetNextPageToken()})
	require.NoError(t, err)
	assert.Len(t, resp2.GetTargets(), 2)
}

// =========================================================================
// CheckTarget
// =========================================================================

func TestCheckTarget_Cached(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	require.NoError(t, err)

	resp, err := svc.CheckTarget(ctx, &pb.CheckTargetRequest{Id: "t1"})
	require.NoError(t, err)
	assert.Equal(t, "t1", resp.GetTarget().GetId())
	assert.False(t, resp.GetReachable()) // default false
	assert.True(t, resp.GetCheckedAt() > 0)
}

func TestCheckTarget_FreshNoFactory(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.AddTarget(ctx, &pb.AddTargetRequest{Id: "t1", Hostname: "h1"})
	require.NoError(t, err)

	// Fresh=true but no factory — should return cached state with a note.
	resp, err := svc.CheckTarget(ctx, &pb.CheckTargetRequest{Id: "t1", Fresh: true})
	require.NoError(t, err)
	assert.False(t, resp.GetReachable())
}

func TestCheckTarget_NotFound(t *testing.T) {
	svc := newTestTargetService(t)
	ctx := context.Background()

	_, err := svc.CheckTarget(ctx, &pb.CheckTargetRequest{Id: "nope"})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}

func TestCheckTarget_EmptyID(t *testing.T) {
	svc := newTestTargetService(t)
	_, err := svc.CheckTarget(context.Background(), &pb.CheckTargetRequest{Id: ""})
	require.Error(t, err)
}

// Ensure emptypb is used.
var _ = emptypb.Empty{}
