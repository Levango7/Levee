// batch_summary_state_test.go pins the BatchSummary method behind the cluster
// v2 observability view. SQLite only — PG is identical SQL, covered by the
// shared logic and CI's integration job.
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRunForBatchTest(t *testing.T, ctx context.Context, store *SQLiteStore, runID string) {
	t.Helper()
	require.NoError(t, store.CreateRun(ctx, &Run{
		ID: runID, WorkflowName: "wf", TemplateName: "tpl", Params: "{}",
		PlanHash: "ph", Status: "running", ApprovalStatus: "approved",
		Creator: "test",
	}))
}

func TestBatchSummary_SQLITEAllTerminal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	runID := "r-all-done"
	seedRunForBatchTest(t, ctx, store, runID)

	// All batches terminal → current_batch_no = 0, done = total.
	for i := 1; i <= 3; i++ {
		require.NoError(t, store.CreateBatch(ctx, &Batch{
			ID:         "b" + string(rune('0'+i)),
			RunID:      runID,
			BatchNo:    i,
			Status:     BatchStateDone,
			TotalHosts: 2,
			Succeeded:  2,
		}))
	}

	summary, err := store.BatchSummary(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 3, summary.TotalBatches)
	assert.Equal(t, 3, summary.DoneBatches)
	assert.Equal(t, 0, summary.CurrentBatchNo, "all terminal → no current batch")
}

func TestBatchSummary_SQLITECurrentBatch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	runID := "r-progress"
	seedRunForBatchTest(t, ctx, store, runID)

	// Batch 1 done, batch 2 running, batch 3 pending.
	_, _ = BatchStateRunning, BatchStatePending
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b1", RunID: runID, BatchNo: 1, Status: BatchStateDone, TotalHosts: 1, Succeeded: 1}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b2", RunID: runID, BatchNo: 2, Status: BatchStateRunning, TotalHosts: 2, Succeeded: 1}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b3", RunID: runID, BatchNo: 3, Status: BatchStatePending, TotalHosts: 3}))

	summary, err := store.BatchSummary(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 3, summary.TotalBatches)
	assert.Equal(t, 1, summary.DoneBatches)
	assert.Equal(t, 2, summary.CurrentBatchNo, "first non-terminal is batch 2")
	assert.Len(t, summary.Batches, 3)
}

func TestBatchSummary_SQLITENoBatches(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	summary, err := store.BatchSummary(ctx, "nonexistent")
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 0, summary.TotalBatches)
	assert.Equal(t, 0, summary.CurrentBatchNo)
}

func TestBatchSummary_SQLITEInterruptedCountsAsNotDone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	runID := "r-interrupted"
	seedRunForBatchTest(t, ctx, store, runID)

	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b1", RunID: runID, BatchNo: 1, Status: BatchStateDone, TotalHosts: 1, Succeeded: 1}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{ID: "b2", RunID: runID, BatchNo: 2, Status: BatchStateInterrupted, TotalHosts: 2, Succeeded: 1, Failed: 1}))

	summary, err := store.BatchSummary(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 2, summary.TotalBatches)
	assert.Equal(t, 1, summary.DoneBatches, "interrupted is not counted as done")
	assert.Equal(t, 2, summary.CurrentBatchNo, "interrupted is the resume point")
}
