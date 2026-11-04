// trace_paging_test.go covers TraceFilter.Offset, the clause that lets a caller
// walk a trace listing in bounded pages.
//
// It exists because ListAuditTraces used to read only the first page and then
// paginate over that slice, so rows past the cap were unreachable — and because
// an offset over an unstable order would silently duplicate or drop rows.
package state

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedTraceRows writes n traces for a run with strictly increasing timestamps,
// so the store's (timestamp, id) order equals insertion order and a paging bug
// cannot hide behind a tie.
func seedTraceRows(t *testing.T, store Store, runID string, n int) []string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, store.CreateRun(ctx, &Run{
		ID:        runID,
		Status:    "completed",
		Creator:   "tester",
		CreatedAt: now,
		UpdatedAt: now,
	}))

	ids := make([]string, 0, n)
	for i := range n {
		id := fmt.Sprintf("%s-tr%04d", runID, i)
		ids = append(ids, id)
		require.NoError(t, store.CreateTrace(ctx, &Trace{
			ID:        id,
			RunID:     runID,
			Event:     "step.done",
			Actor:     "tester",
			Timestamp: now.Add(time.Duration(i) * time.Second),
		}))
	}
	return ids
}

func TestListTracesOffsetPagesCoverEveryRowOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const total = 7
	const pageSize = 3
	want := seedTraceRows(t, store, "paging", total)

	// Fixture premise: the rows really do span several pages of size 3.
	full, err := store.ListTraces(ctx, TraceFilter{RunID: "paging"})
	require.NoError(t, err)
	require.Len(t, full, total, "未加 Limit 的读取必须拿到全部行")

	// A bounded loop on purpose: if OFFSET were ignored by the query builder,
	// every page would return the same first rows, and a "loop until short page"
	// would spin until the test binary times out instead of reporting a failure.
	pageCount := (total + pageSize - 1) / pageSize
	var got []string
	for page := range pageCount {
		rows, perr := store.ListTraces(ctx, TraceFilter{RunID: "paging", Limit: pageSize, Offset: page * pageSize})
		require.NoError(t, perr)
		wantLen := min(pageSize, total-page*pageSize)
		require.Len(t, rows, wantLen, "第 %d 页的行数不对", page)
		for _, tr := range rows {
			got = append(got, tr.ID)
		}
	}
	assert.Equal(t, want, got, "按 (timestamp, id) 翻页既不重复也不漏，且顺序等于插入序")
}

func TestListTracesOffsetIgnoredWithoutLimit(t *testing.T) {
	// SQLite rejects OFFSET without LIMIT, so the two travel together in the
	// query builder. An offset handed over without a limit must therefore read
	// the whole run rather than fail or silently skip rows.
	store := newTestStore(t)
	ctx := context.Background()

	seedTraceRows(t, store, "no-limit", 4)

	rows, err := store.ListTraces(ctx, TraceFilter{RunID: "no-limit", Offset: 2})
	require.NoError(t, err)
	assert.Len(t, rows, 4, "没有 Limit 时 Offset 不参与构造 SQL")
}
