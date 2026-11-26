package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

func TestListChangesTotalSizeIsTheWholeVisibleCount(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 25; i++ {
		status := "pending"
		if i >= 23 {
			status = "completed"
		}
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: fmt.Sprintf("run-count-%02d", i), WorkflowName: "wf", Status: status,
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now, Creator: "test",
		}))
	}

	first, err := svc.ListChanges(ctx, &pb.ListChangesRequest{PageSize: 20})
	require.NoError(t, err)
	assert.Len(t, first.GetChanges(), 20)
	assert.Equal(t, int32(25), first.GetTotalSize())
	require.NotEmpty(t, first.GetNextPageToken())

	second, err := svc.ListChanges(ctx, &pb.ListChangesRequest{PageSize: 20, PageToken: first.GetNextPageToken()})
	require.NoError(t, err)
	assert.Len(t, second.GetChanges(), 5)
	assert.Equal(t, int32(25), second.GetTotalSize())
	assert.Empty(t, second.GetNextPageToken())

	pending, err := svc.ListChanges(ctx, &pb.ListChangesRequest{Statuses: []string{"pending"}, PageSize: 1})
	require.NoError(t, err)
	assert.Len(t, pending.GetChanges(), 1)
	assert.Equal(t, int32(23), pending.GetTotalSize(), "KPI pageSize=1 must still see all 23 pending changes")
}

func TestListChangesTotalSizeWithHundredRowsAndComplexFilters(t *testing.T) {
	svc, store := newTestChangeService(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 100; i++ {
		status := "failed"
		if i%2 == 0 {
			status = "completed"
		}
		label := "noise"
		if i < 30 {
			label = fmt.Sprintf("needle-%02d", i)
		}
		require.NoError(t, store.CreateRun(ctx, &state.Run{
			ID: fmt.Sprintf("run-complex-%03d", i), WorkflowName: label, Status: status,
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now, Creator: "test",
		}))
	}

	req := &pb.ListChangesRequest{
		Statuses: []string{"failed", "completed"}, LabelContains: "needle", PageSize: 10,
	}
	first, err := svc.ListChanges(ctx, req)
	require.NoError(t, err)
	assert.Len(t, first.GetChanges(), 10)
	assert.Equal(t, int32(30), first.GetTotalSize())
	require.NotEmpty(t, first.GetNextPageToken())

	second, err := svc.ListChanges(ctx, &pb.ListChangesRequest{
		Statuses: []string{"failed", "completed"}, LabelContains: "needle", PageSize: 10,
		PageToken: first.GetNextPageToken(),
	})
	require.NoError(t, err)
	assert.Len(t, second.GetChanges(), 10)
	assert.Equal(t, int32(30), second.GetTotalSize())
}

func TestListChangesTotalSizeHonoursReadVisibility(t *testing.T) {
	ctx := context.Background()
	svc, _ := policySvc(t)
	createIn(t, svc, "alice", "dev")
	createIn(t, svc, "alice", "dev")
	createIn(t, svc, "alice", "prod")

	resp, err := svc.ListChanges(ContextWithActor(ctx, "bob"), &pb.ListChangesRequest{PageSize: 1})
	require.NoError(t, err)
	assert.Len(t, resp.GetChanges(), 1)
	assert.Equal(t, int32(2), resp.GetTotalSize(), "count both visible dev rows, excluding prod")
}
