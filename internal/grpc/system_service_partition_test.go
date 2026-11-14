package grpc

// system_service_partition_test.go — 断网策略 Q3：分区期的状态面必须带标记，
// 且**不得**把「读不到」说成「0 个活跃执行」。
//
// 背景（docs/design-disconnect-policy.md §2.3 / §3 第 2 条）：Q3 拍板的是
// 「只读白名单」，白名单里的读在分区期仍可回答，但回答必须带「PG 不可达」
// 标记，而不是假装新鲜。这条守卫钉住的就是那个「假装」：
//
//	① 控制面不可达 → control_plane_reachable=false、run_counts_available=false，
//	   分区原因非空，状态降级，且**不读** run 表（读了只会拿到错误）；
//	② 控制面可达 → 两个标志都为 true，计数照常；
//	③ 可达但计数读取失败 → 控制面可达（true），但计数不可用（false），
//	   区分「数据库断了」与「这次没读到」。
//
// 没有这条守卫时，GetStatus 的老行为会在数据库挂掉时安静地返回
// active_runs=0 —— 一个关于平台的**肯定陈述**，而事实是「看不到」。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/nexus/levee/internal/config"
	"github.com/nexus/levee/internal/diagnosis"
	"github.com/nexus/levee/internal/partition"
)

// unreachablePinger is a control plane that stopped answering.
type unreachablePinger struct{ err error }

func (p unreachablePinger) Ping(context.Context) error { return p.err }

// countingPinger answers, and counts how many times it was asked.
type countingPinger struct{ calls int }

func (p *countingPinger) Ping(context.Context) error { p.calls++; return nil }

func newStatusService(t *testing.T, checker *partition.Checker) *SystemService {
	t.Helper()
	return NewSystemService(nil, &config.Config{}, "", "v", "c", "b", "go", time.Now()).
		WithPartitionChecker(checker)
}

func TestGetStatusMarksThePartitionInsteadOfReportingZero(t *testing.T) {
	probeErr := errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")
	svc := newStatusService(t, partition.NewChecker(unreachablePinger{err: probeErr}, time.Second))

	resp, err := svc.GetStatus(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)

	assert.False(t, resp.ControlPlaneReachable,
		"a partitioned serve must say the control plane is unreachable")
	assert.False(t, resp.RunCountsAvailable,
		"run counts are unknown during a partition — not zero")
	assert.Contains(t, resp.PartitionReason, "connection refused",
		"the probe's own text must reach the operator")
	assert.Equal(t, string(diagnosis.StatusDegraded), resp.Status,
		"a partitioned platform is not healthy")
	assert.NotEmpty(t, resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "control plane database unreachable")
}

func TestGetStatusProbesFreshlyOnEveryCall(t *testing.T) {
	pinger := &countingPinger{}
	svc := newStatusService(t, partition.NewChecker(pinger, time.Second))

	_, err := svc.GetStatus(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)
	_, err = svc.GetStatus(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)

	assert.Equal(t, 2, pinger.calls,
		"the probe must be per-request: a cached \"reachable\" is a refusal to refuse")
}

func TestGetStatusWithoutACheckerKeepsHistoricalBehaviour(t *testing.T) {
	// No checker installed (the single-node SQLite path): no partition to
	// report, and the counts are read straight from the store — which is nil
	// here, so the surface says "store not configured" rather than pretending.
	svc := NewSystemService(nil, &config.Config{}, "", "v", "c", "b", "go", time.Now())

	resp, err := svc.GetStatus(context.Background(), &emptypb.Empty{})
	require.NoError(t, err)

	assert.True(t, resp.ControlPlaneReachable,
		"without a probe there is no partition to declare")
	assert.False(t, resp.RunCountsAvailable,
		"no store means no counts — the surface must not claim a fresh reading")
	assert.Contains(t, resp.Warnings, "store not configured")
}
