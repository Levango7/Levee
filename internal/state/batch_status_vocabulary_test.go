// batch_status_vocabulary_test.go — batchDoneStates 必须认识执行端真正写入的值。
//
// 这条守卫存在的原因是一次真实的口径分裂：internal/wiring/persist.go 把批次写成
// "completed"/"failed"，而这里判"完成"的集合里只有 "done"。两边各自都有测试——
// state 包的用例自己按 BatchStateDone 种数据（读的正是它写的那个词），wiring 包的
// 用例根本不看 BatchSummary——所以整套测试是绿的，而线上真实的 run 报
// DoneBatches=0、CurrentBatchNo 停在第一批。
//
// 下面两条用例按"写入端的字面量"来种数据。它们对旧实现必然变红，因此是这条缝
// 在 owning 包内的守卫；跨缝的那条在 internal/wiring/batch_summary_seam_test.go。
package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBatchSummary_CompletedCountsAsDone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	runID := "r-completed"
	seedRunForBatchTest(t, ctx, store, runID)

	// 值取自执行端实际写的那两个字符串（internal/wiring/persist.go）。
	require.NoError(t, store.CreateBatch(ctx, &Batch{
		ID: "bc-1", RunID: runID, BatchNo: 1,
		Status: BatchStateCompleted, TotalHosts: 2, Succeeded: 2,
	}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{
		ID: "bc-2", RunID: runID, BatchNo: 2,
		Status: BatchStateCompleted, TotalHosts: 1, Succeeded: 1,
	}))

	summary, err := store.BatchSummary(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Equal(t, 2, summary.DoneBatches,
		"a batch written as completed must count as done — this is the value the engine persists")
	assert.Equal(t, 0, summary.CurrentBatchNo, "nothing left to resume")
}

func TestBatchSummary_CompletedAndFailedMix(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	runID := "r-mix"
	seedRunForBatchTest(t, ctx, store, runID)

	require.NoError(t, store.CreateBatch(ctx, &Batch{
		ID: "bm-1", RunID: runID, BatchNo: 1,
		Status: BatchStateCompleted, TotalHosts: 1, Succeeded: 1,
	}))
	require.NoError(t, store.CreateBatch(ctx, &Batch{
		ID: "bm-2", RunID: runID, BatchNo: 2,
		Status: BatchStateFailed, TotalHosts: 1, Failed: 1,
	}))

	summary, err := store.BatchSummary(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	// failed 刻意不算 done：它是要重跑的那一批，续跑点因此落在它身上。
	assert.Equal(t, 1, summary.DoneBatches)
	assert.Equal(t, 2, summary.CurrentBatchNo, "the failed batch is the resume point")
}

func TestBatchStateConstantsMatchTheWrittenStrings(t *testing.T) {
	// 常量与执行端字面量的对应关系钉在这里：改任何一边都会让另一边的测试变红，
	// 而不是让读写口径悄悄重新分家。
	assert.Equal(t, "completed", BatchStateCompleted)
	assert.Equal(t, "failed", BatchStateFailed)
	assert.Equal(t, "rolled_back", BatchStateRolledBack)

	// done 集合的成员关系也是契约的一部分：completed 必须在内，failed/rolled_back
	// 必须不在（前者未了结、后者不是前向完成）。
	for _, s := range []string{BatchStateCompleted, BatchStateDone} {
		assert.True(t, batchDoneStates[s], s+" must count as done")
	}
	for _, s := range []string{BatchStateFailed, BatchStateRolledBack, BatchStateInterrupted, BatchStatePending, BatchStateRunning} {
		assert.False(t, batchDoneStates[s], s+" must not be treated as a completed forward batch")
	}
}
