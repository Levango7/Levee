// inventory_service_test.go — exercises InventoryService against a real
// SQLite store: group lifecycle, YAML import, status transitions and
// target history.
package grpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestInventoryService(t *testing.T) *InventoryService {
	t.Helper()
	return NewInventoryService(newTestStore(t))
}

func TestInventoryGroupLifecycle(t *testing.T) {
	svc := newTestInventoryService(t)
	ctx := context.Background()

	g, err := svc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "prod/db"})
	require.NoError(t, err)
	assert.Equal(t, "prod/db", g.GetName())

	_, err = svc.CreateGroup(ctx, &pb.CreateGroupRequest{Name: "prod"})
	require.NoError(t, err)

	listed, err := svc.ListGroups(ctx, &pb.ListGroupsRequest{})
	require.NoError(t, err)
	assert.Len(t, listed.GetGroups(), 2)

	// Deleting a group that still holds targets is rejected.
	require.NoError(t, svc.store.UpsertTarget(ctx, &state.Target{
		ID: "tgt-h", Hostname: "h1", Port: 22, GroupID: g.GetId(),
	}))
	_, err = svc.DeleteGroup(ctx, &pb.DeleteGroupRequest{Id: g.GetId()})
	require.Error(t, err)

	require.NoError(t, svc.store.DeleteTarget(ctx, "tgt-h"))
	_, err = svc.DeleteGroup(ctx, &pb.DeleteGroupRequest{Id: g.GetId()})
	require.NoError(t, err)
}

func TestInventoryImportAndStatus(t *testing.T) {
	svc := newTestInventoryService(t)
	ctx := context.Background()

	resp, err := svc.ImportTargets(ctx, &pb.ImportTargetsRequest{
		YamlContent: "targets:\n  - address: 10.5.5.5\n  - address: 10.5.5.6\n    group: edge\n",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(2), resp.GetCreated())
	assert.Empty(t, resp.GetErrors())

	st, err := svc.SetTargetStatus(ctx, &pb.SetTargetStatusRequest{
		TargetId: findTargetID(t, svc, "10.5.5.5"), Status: state.StatusFrozen,
	})
	require.NoError(t, err)
	assert.Equal(t, state.StatusFrozen, st.GetStatus())
}

// findTargetID looks up a target's ID by hostname via the store.
func findTargetID(t *testing.T, svc *InventoryService, host string) string {
	t.Helper()
	targets, err := svc.store.ListTargets(context.Background(), state.TargetFilter{})
	require.NoError(t, err)
	for _, tg := range targets {
		if tg.Hostname == host {
			return tg.ID
		}
	}
	t.Fatalf("target with host %q not found", host)
	return ""
}

func TestInventoryTargetHistory(t *testing.T) {
	svc := newTestInventoryService(t)
	ctx := context.Background()

	run := &state.Run{ID: "run-hist", WorkflowName: "nginx-reload", Status: "completed"}
	require.NoError(t, svc.store.CreateRun(ctx, run))
	require.NoError(t, svc.store.CreateBatch(ctx, &state.Batch{
		ID: "b-1", RunID: "run-hist", BatchNo: 1, Status: "success",
	}))
	require.NoError(t, svc.store.CreateStep(ctx, &state.Step{
		ID: "st-1", RunID: "run-hist", BatchID: "b-1", Host: "10.7.7.7",
		StepName: "reload", Status: "success",
	}))

	resp, err := svc.TargetHistory(ctx, &pb.TargetHistoryRequest{Host: "10.7.7.7"})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	entry := resp.GetEntries()[0]
	assert.Equal(t, "run-hist", entry.GetRunId())
	assert.Equal(t, "nginx-reload", entry.GetWorkflowName())

	empty, err := svc.TargetHistory(ctx, &pb.TargetHistoryRequest{Host: "never-touched"})
	require.NoError(t, err)
	assert.Empty(t, empty.GetEntries())
}

// historyDoc is a workflow SOURCE document, not a name. runs.workflow_name
// stores the rendered document, so a history LIST that ships it verbatim puts
// the whole YAML on the wire for every caller. The wire must carry only the
// declared name (docs/blocked-items.md: "只带声明名").
const historyDoc = "name: rolling-patch\nversion: \"1.0\"\n" +
	"target:\n  type: host\n  query: \"env=prod\"\n" +
	"steps:\n  - name: deploy\n    action: shell.exec\n    args:\n      cmd: \"true\"\n"

// TestInventoryTargetHistoryWorkflowNameCarriesDeclaredName: the wire row must
// carry the workflow's DECLARED name, never the rendered document. A path
// (single-line, no newline) is already a name and passes through unchanged.
func TestInventoryTargetHistoryWorkflowNameCarriesDeclaredName(t *testing.T) {
	svc := newTestInventoryService(t)
	ctx := context.Background()

	at := time.Date(2024, 3, 17, 8, 5, 0, 0, time.UTC)
	docRun := &state.Run{ID: "run-doc", WorkflowName: historyDoc, Status: "completed", CreatedAt: at}
	pathRun := &state.Run{ID: "run-path", WorkflowName: "workflows/patch.yaml", Status: "completed", CreatedAt: at}
	for i, r := range []*state.Run{docRun, pathRun} {
		require.NoError(t, svc.store.CreateRun(ctx, r))
		batchID := fmt.Sprintf("hb-%d", i)
		require.NoError(t, svc.store.CreateBatch(ctx, &state.Batch{
			ID: batchID, RunID: r.ID, BatchNo: 1, Status: "success",
		}))
		require.NoError(t, svc.store.CreateStep(ctx, &state.Step{
			ID: fmt.Sprintf("hs-%d", i), RunID: r.ID, BatchID: batchID,
			Host: "10.8.8.8", StepName: "deploy", Status: "success",
		}))
	}

	resp, err := svc.TargetHistory(ctx, &pb.TargetHistoryRequest{Host: "10.8.8.8"})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 2)

	byID := map[string]*pb.TargetHistoryEntry{}
	for _, e := range resp.GetEntries() {
		byID[e.GetRunId()] = e
	}
	doc, ok := byID["run-doc"]
	require.True(t, ok, "doc run must be listed")
	assert.Equal(t, "rolling-patch", doc.GetWorkflowName(),
		"wire must carry the declared name, not the rendered document")
	assert.NotContains(t, doc.GetWorkflowName(), "action: shell.exec",
		"workflow source must not cross the wire")
	path, ok := byID["run-path"]
	require.True(t, ok, "path run must be listed")
	assert.Equal(t, "workflows/patch.yaml", path.GetWorkflowName(),
		"a single-line path is already a name and must pass through unchanged")
}
