package wiring

// batch_summary_seam_test.go — 写批次的那一端和读批次的那一端必须说同一个词。
//
// 缺陷形态（2026-10-07 定位）：persistClosureResults 把批次写成 "completed"，
// 而 internal/state 的 batchDoneStates 只认 "done"。字面上两边都"对"——
// state 包里的测试自己造行、用的就是它自己读的那个字符串，wiring 包里的测试根本不读
// BatchSummary，所以没有任何一条用例跨过这条缝。结果全落在操作者看得见的数字上：
// 一趟全部成功的 run 会报 DoneBatches=0，并且 CurrentBatchNo 停在第一批，
// 读起来像"第一批还没跑完，可以从这里续跑"。
//
// 这条用例就是把"真实写入 → 真实读取"接起来：它对旧实现必然变红，因此它是这条缝
// 以后不会再次无声裂开的守卫。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/batch"
	"github.com/nexus/levee/internal/engine"
	"github.com/nexus/levee/internal/plan"
	"github.com/nexus/levee/internal/state"
)

func TestPersistedBatchesAreCountedDoneByBatchSummary(t *testing.T) {
	ctx := context.Background()
	e, store := newTestEngine(t, "web-1", "web-2")
	seedRun(t, store, "run-seam-1", "name: seam\n")

	p := &plan.Plan{Batches: []plan.Batch{
		{Index: 0, Targets: []string{"web-1"}},
		{Index: 1, Targets: []string{"web-2"}},
	}}
	res := &engine.ClosureResult{BatchResults: []*batch.BatchResult{
		{BatchIndex: 0, TargetResults: []batch.TargetResult{{Target: "web-1"}}},
		{
			BatchIndex:    1,
			TargetResults: []batch.TargetResult{{Target: "web-2", Error: errors.New("step failed")}},
			Error:         errors.New("step failed"),
		},
	}}
	require.NoError(t, e.persistClosureResults(ctx, "run-seam-1", p, res, nil))

	sum, err := store.BatchSummary(ctx, "run-seam-1")
	require.NoError(t, err)
	require.NotNil(t, sum, "the run has batch rows, so a summary must come back")

	assert.Equal(t, 2, sum.TotalBatches)
	// 只有成功的第 1 批算"彻底完成"。failed 按 batchDoneStates 的设计不算——
	// 它是待重跑的那一批，不是已了结的那一批。
	assert.Equal(t, 1, sum.DoneBatches,
		"a batch persisted as completed must be counted done; this is the drift this test exists to hold")
	// 续跑目标因此是第 2 批，不是"看起来还没跑"的第 1 批。
	assert.Equal(t, 2, sum.CurrentBatchNo,
		"CurrentBatchNo must point at the batch that still needs work")

	// 再看写入端本身：落库的字符串必须是共享常量，而不是任何一边的字面量。
	rows, err := store.ListBatches(ctx, state.BatchFilter{RunID: "run-seam-1"})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, state.BatchStateCompleted, rows[0].Status)
	assert.Equal(t, state.BatchStateFailed, rows[1].Status)
}

func TestPersistedRollbackBatchIsNotCountedDone(t *testing.T) {
	ctx := context.Background()
	_, store := newTestEngine(t, "web-1")
	seedRun(t, store, "run-seam-2", "name: seam\n")

	require.NoError(t, store.CreateBatch(ctx, &state.Batch{
		ID: "bat-seed-1", RunID: "run-seam-2", BatchNo: 1,
		Status: state.BatchStateRolledBack, TotalHosts: 1,
	}))

	sum, err := store.BatchSummary(ctx, "run-seam-2")
	require.NoError(t, err)
	require.NotNil(t, sum)
	// rolled_back 不是"前向完成"：把它算进 DoneBatches 会让一趟回滚的 run
	// 看起来全都跑完了。这条断言把边界钉住，防止有人为了凑数字把它塞进 done 集合。
	assert.Zero(t, sum.DoneBatches, "a rolled-back batch is not a completed forward batch")
	assert.Equal(t, 1, sum.CurrentBatchNo)
}
