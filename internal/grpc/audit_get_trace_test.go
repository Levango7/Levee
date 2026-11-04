// audit_get_trace_test.go pins GetAuditTrace — the RPC behind
// `levee audit show <trace-id>` — on the two things that are not "it returned
// something": the entry must be the STORED row, and an unusable id must be an
// argument error rather than a scan or a 500.
//
// "The stored row" includes the fraction of a second the recorder stamped.
// TraceEntry.timestamp is Unix seconds, and every fixture this repo had was
// built with Truncate(time.Second), so a mapping that dropped the remainder was
// untestable rather than correct — while the rows a real run writes carry it
// (internal/audit/trace.go stamps with time.Now(), and both dialects keep the
// fraction: SQLite nanoseconds, PostgreSQL microseconds). Field 15
// (timestamp_nanos) is that remainder.
//
// The permission-matrix side of this RPC lives in service_policy_test.go with
// the other audit reads, where the seeded dev/prod runs already are.
package grpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexus/levee/internal/grpc/pb"
	"github.com/nexus/levee/internal/state"
)

func TestGetAuditTraceReturnsTheStoredRow(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()
	seedRun(t, ctx, store, "show-run", "completed")

	// Deliberately not second-aligned: this is the shape a real run writes.
	stamp := time.Date(2024, 3, 17, 8, 5, 3, 123456789, time.UTC)
	stored := `{"target":"host:web-07","duration_ms":1200,"metadata":{"batch":"3"}}`
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID:        "show-tr-1",
		RunID:     "show-run",
		Event:     "step.done",
		Actor:     "tester",
		Detail:    stored,
		Timestamp: stamp,
		// Not empty on purpose: with the column unset the mapping of tenant_id
		// would go untested, and it is part of the row the CLI prints.
		TenantID: "t-a",
	}))

	// Fixture premise, stated rather than assumed: the store really did keep the
	// fraction. Without this, a failure below could be the database's doing and
	// the assertion would be blaming the mapping for nothing.
	back, err := store.GetTrace(ctx, "show-tr-1")
	require.NoError(t, err)
	require.Equal(t, stamp.UnixNano(), back.Timestamp.UnixNano(),
		"夹具必须真的带亚秒，否则本用例只是在测自己")

	got, err := svc.GetAuditTrace(ctx, &pb.GetAuditTraceRequest{TraceId: "show-tr-1"})
	require.NoError(t, err)

	assert.Equal(t, "show-tr-1", got.GetId())
	assert.Equal(t, "show-run", got.GetRunId())
	assert.Equal(t, "step.done", got.GetAction())
	assert.Equal(t, "tester", got.GetActor())
	assert.Equal(t, "t-a", got.GetTenantId())
	assert.Equal(t, stored, got.GetStoredDetail(), "入库原文逐字节上线")

	assert.Equal(t, stamp.Unix(), got.GetTimestamp())
	assert.Equal(t, int32(stamp.Nanosecond()), got.GetTimestampNanos(),
		"亚秒余数必须上线，否则 CLI 重建出的时间戳不是账本里那一个")

	// The projection fields keep their existing meaning; adding field 15 is not
	// a licence to change what field 12 renders.
	assert.Equal(t, `{"batch":"3"}`, got.GetDetail(), "控制台投影照旧")
	assert.Equal(t, "web-07", got.GetTargetHost(), "host: 前缀照旧剥掉")
	assert.Equal(t, int64(1200), got.GetDurationMs())
}

func TestGetAuditTraceRefusesAnUnusableId(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()
	seedRun(t, ctx, store, "arg-run", "completed")
	require.NoError(t, store.CreateTrace(ctx, &state.Trace{
		ID: "arg-tr-1", RunID: "arg-run", Event: "step.done",
		Timestamp: time.Now().UTC().Truncate(time.Second),
	}))

	_, err := svc.GetAuditTrace(ctx, nil)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err),
		"nil 请求是调用方的错，不该报 Internal")

	_, err = svc.GetAuditTrace(ctx, &pb.GetAuditTraceRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err),
		"空 id 不能被当成「不过滤」——那是一次全表扫描，而且是任意一行的答案")

	_, err = svc.GetAuditTrace(ctx, &pb.GetAuditTraceRequest{TraceId: "arg-tr-nope"})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err), "查无此行就说查无此行")
}

// TestGetAuditLogCarriesTheSubSecondRemainder covers the OTHER producer of
// TraceEntry. auditToTraceEntry maps audit rows, not trace rows, and field 15
// means "this row's sub-second remainder" whichever row it is: a producer that
// left it at zero would hand the next reader of /audit/log a timestamp silently
// up to a second off, with nothing in the message to say so.
func TestGetAuditLogCarriesTheSubSecondRemainder(t *testing.T) {
	svc, store := newTestAuditService(t)
	ctx := context.Background()
	seedRun(t, ctx, store, "log-run", "completed")

	stamp := time.Date(2024, 3, 17, 8, 5, 3, 987654321, time.UTC)
	require.NoError(t, store.CreateAudit(ctx, &state.Audit{
		ID: "log-aud-1", RunID: "log-run", Action: "apply", Actor: "alice",
		Target: "web-1", Result: "ok", Timestamp: stamp,
	}))

	back, err := store.GetAudit(ctx, "log-aud-1")
	require.NoError(t, err)
	require.Equal(t, stamp.UnixNano(), back.Timestamp.UnixNano(),
		"夹具必须真的带亚秒，否则本用例只是在测自己")

	resp, err := svc.GetAuditLog(ctx, &pb.GetAuditLogRequest{RunId: "log-run"})
	require.NoError(t, err)
	require.Len(t, resp.GetEntries(), 1)
	entry := resp.GetEntries()[0]

	assert.Equal(t, stamp.Unix(), entry.GetTimestamp())
	assert.Equal(t, int32(stamp.Nanosecond()), entry.GetTimestampNanos(),
		"审计行与 trace 行共用同一条消息，亚秒余数不能只有一边填")
}
