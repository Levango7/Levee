// audit_traces_paging_test.go pins the ListAuditTraces read path against the
// store page cap.
//
// The handler used to fetch at most maxPageSize rows (helpers.go: 1000) and then
// paginate over that slice. A run with more traces than the cap was therefore
// unreachable past it — silently: the CLI's `audit list`/`audit export` read the
// whole run locally, so the two modes disagreed without saying so. These tests
// cross the cap on purpose and assert the exact total too, because the old code
// reported the capped length as if it were the whole ledger. The fix walks the
// store in auditStorePageSize chunks, so the fixture has to stay above both
// constants; the premise below says so out loud rather than trusting that the
// two happen to be 1000 each.
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

// seedTracesAcrossCap writes auditStorePageSize+7 traces for runID and returns
// their ids in the order the store lists them (timestamp ASC, id ASC).
func seedTracesAcrossCap(t *testing.T, ctx context.Context, store state.Store, runID string) []string {
	t.Helper()
	seedRun(t, ctx, store, runID, "completed")

	const total = auditStorePageSize + 7
	base := time.Now().UTC().Truncate(time.Second)
	ids := make([]string, 0, total)
	for i := range total {
		id := fmt.Sprintf("%s-tr%05d", runID, i)
		ids = append(ids, id)
		require.NoError(t, store.CreateTrace(ctx, &state.Trace{
			ID:        id,
			RunID:     runID,
			Event:     "step.done",
			Actor:     "tester",
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}))
	}
	return ids
}

// collectTraces walks every wire page, the way the CLI does.
func collectTraces(ctx context.Context, t *testing.T, svc *AuditService, req *pb.ListAuditTracesRequest) []*pb.TraceEntry {
	t.Helper()
	var out []*pb.TraceEntry
	for {
		resp, err := svc.ListAuditTraces(ctx, req)
		require.NoError(t, err)
		out = append(out, resp.GetEntries()...)
		token := resp.GetNextPageToken()
		if token == "" {
			return out
		}
		req = &pb.ListAuditTracesRequest{
			RunIds:    req.GetRunIds(),
			PageSize:  req.GetPageSize(),
			PageToken: token,
		}
	}
}

func TestListAuditTracesReadsPastTheStorePageCap(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	want := seedTracesAcrossCap(t, ctx, store, "big-run")
	const total = auditStorePageSize + 7

	// Fixture premise, stated rather than assumed: the unbounded read really is
	// longer than the cap, and a capped read really does stop at the cap. This is
	// what the handler used to do, and the only reason this test bites.
	capped, err := store.ListTraces(ctx, state.TraceFilter{RunID: "big-run", Limit: auditStorePageSize})
	require.NoError(t, err)
	require.Len(t, capped, auditStorePageSize, "夹具必须真的跨过页上限，否则本用例只是在测自己")
	unbounded, err := store.ListTraces(ctx, state.TraceFilter{RunID: "big-run"})
	require.NoError(t, err)
	require.Len(t, unbounded, total)
	require.Greater(t, total, maxPageSize,
		"行数也要超过旧代码那次单次读取的上限，否则这条用例抓不到当初的截断")

	t.Run("具名 run", func(t *testing.T) {
		got := collectTraces(ctx, t, svc, &pb.ListAuditTracesRequest{
			RunIds:   []string{"big-run"},
			PageSize: 400,
		})
		require.Len(t, got, total, "翻页必须拿到全部行")
		ids := make([]string, 0, len(got))
		for _, e := range got {
			ids = append(ids, e.GetId())
		}
		assert.Equal(t, want, ids, "跨页之后顺序仍等于存储顺序")
	})

	t.Run("不加 run 过滤", func(t *testing.T) {
		got := collectTraces(ctx, t, svc, &pb.ListAuditTracesRequest{PageSize: 400})
		assert.Len(t, got, total, "全局枚举同样不许在页上限处静默收尾")
	})
}

func TestListAuditTracesTotalSizeIsTheFilteredCount(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedTracesAcrossCap(t, ctx, store, "total-run")
	const total = auditStorePageSize + 7

	resp, err := svc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{
		RunIds:   []string{"total-run"},
		PageSize: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, int32(total), resp.GetTotalSize(),
		"total_size 是过滤后的全量行数，不是本页也不是页上限")
	assert.Len(t, resp.GetEntries(), 10, "页大小仍按请求收窄")
}

func TestListAuditTracesCarriesTheStoredDetailVerbatim(t *testing.T) {
	// The console projection in TraceEntry.detail re-renders the row's payload
	// ("error: …" or the recorder's metadata). `audit export` and `audit show`
	// print the stored bytes — the ones the hash chain covers — so they need a
	// field that is not a projection.
	svc, store := newTestAuditService(t)
	ctx := context.Background()

	seedRun(t, ctx, store, "detail-run", "completed")
	stored := `{"step":3,"target":"host:web-01","metadata":{"batch":"2"}}`
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "detail-tr-1",
		RunID:     "detail-run",
		Event:     "step.done",
		Actor:     "tester",
		Detail:    stored,
		Timestamp: time.Now().UTC().Truncate(time.Second),
		// Not empty on purpose: with the column unset, the mapping of tenant_id
		// would go untested — and it is part of the row the CLI prints.
		TenantID: "t-a",
	}))

	resp, err := svc.ListAuditTraces(ctx, &pb.ListAuditTracesRequest{RunIds: []string{"detail-run"}})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	entry := resp.GetEntries()[0]

	assert.Equal(t, stored, entry.GetStoredDetail(), "入库原文逐字节上线")
	assert.Equal(t, `{"batch":"2"}`, entry.GetDetail(), "控制台投影保持原样，不随这次加字段而变")
	assert.Equal(t, "t-a", entry.GetTenantId(), "租户列也要上线，导出才谈得上等于本地")
}
