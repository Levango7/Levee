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
	"os"
	"path/filepath"
	"regexp"
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

// The UI keeps a hand-written mirror of this vocabulary (web/src/utils/batch.ts
// lists the spellings it colours and labels). Copies drift, and this particular
// copy already caused one shipped defect: state judged "done" by a string the
// engine never wrote. internal/runstatus/vocabulary_guard_test.go does the same
// job for run statuses; this is the batch-level half, kept in the owning package
// so a constant added here without a UI mapping turns red *here*.

// repoFile walks up from the test working directory to locate a repository file,
// so the guard works from any package depth.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("cannot locate %s from the test working directory", rel)
	return ""
}

// goBatchStates parses the constant block in store.go instead of restating it.
// A restatement would let a newly added constant pass unnoticed.
func goBatchStates(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile(repoFile(t, "internal/state/store.go"))
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^\s*(BatchState\w+)\s*=\s*"([^"]+)"$`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = m[2]
	}
	require.NotEmpty(t, out, "no BatchState constants parsed from store.go — the const block shape changed")
	return out
}

func TestWebBatchMirrorMatchesGoBatchStates(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "web/src/utils/batch.ts"))
	require.NoError(t, err)
	text := string(src)

	decl := regexp.MustCompile(`(?m)^export const BATCH_STATES = \[([^\]]*)\] as const$`).FindStringSubmatch(text)
	require.Len(t, decl, 2, "could not find the `export const BATCH_STATES = [...] as const` declaration")
	uiStates := map[string]bool{}
	// Both quote styles: this file is single-quoted today, but the guard should
	// not go red because someone ran a formatter that prefers `"`.
	for _, m := range regexp.MustCompile(`['"]([^'"]+)['"]`).FindAllStringSubmatch(decl[1], -1) {
		uiStates[m[1]] = true
	}
	require.NotEmpty(t, uiStates, "BATCH_STATES parsed empty")

	// Direction 1: every state Go can write has a UI mapping. Without this the
	// page shows the raw wire value and, worse, batchTagType falls back to grey.
	goStates := map[string]bool{}
	for _, v := range goBatchStates(t) {
		goStates[v] = true
		assert.True(t, uiStates[v], "batch state "+v+" has no entry in web/src/utils/batch.ts BATCH_STATES")
	}
	// Direction 2: the UI does not invent a spelling the backend cannot produce.
	// That is how `success` and a bare `done` once lived here for years.
	for s := range uiStates {
		assert.True(t, goStates[s], "web BATCH_STATES lists "+s+", which no BatchState constant declares")
	}
	assert.Equal(t, len(goStates), len(uiStates), "the two vocabularies differ in size, so one side has an entry the other lacks")
}

func TestWebBatchLabelsCoverEveryState(t *testing.T) {
	src, err := os.ReadFile(repoFile(t, "web/src/utils/batch.ts"))
	require.NoError(t, err)

	block := regexp.MustCompile(`(?m)^const LABEL_BY_STATE: Record<BatchState, string> = \{\n([\s\S]*?)^\}$`).FindStringSubmatch(string(src))
	require.Len(t, block, 2, "could not find the `LABEL_BY_STATE: Record<BatchState, string>` map — its shape changed")
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+):`).FindAllStringSubmatch(block[1], -1) {
		keys[m[1]] = true
	}
	require.NotEmpty(t, keys, "LABEL_BY_STATE parsed empty")

	for name, value := range goBatchStates(t) {
		assert.True(t, keys[value], "%s (%q) has no batchLabel entry — the Chinese page would render %q verbatim", name, value, value)
	}
}
