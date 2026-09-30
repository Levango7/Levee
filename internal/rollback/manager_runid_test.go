package rollback

// manager_runid_test.go — RollbackResult.RunID 的落戳契约。
//
// 这条字段存在的唯一理由是「可归属」：回滚之后的一切（验证、分级、通知、
// 升级）只拿到一个 *RollbackResult，没有别的地方能说出这是哪一次运行。
// 因此它必须在每一条返回路径上都被落上，包括连计划都拿不到的那种失败。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nexus/levee/internal/plan"
)

func TestRollbackResultCarriesRunID(t *testing.T) {
	mgr := NewManager(WithWhitelistAll(), WithRunID("run-77"))
	require.NotNil(t, mgr)

	res := mgr.RollbackWithLedger(context.Background(), &plan.Plan{}, nil, nil)
	require.NotNil(t, res)
	assert.Equal(t, "run-77", res.RunID)
}

func TestRollbackResultRunIDStampedOnNilPlan(t *testing.T) {
	// 连计划都没有的回滚是最需要被关联到某次运行的那一种：操作员看到的是
	// 「回滚失败了」，却不知道是哪一次。
	mgr := NewManager(WithRunID("run-99"))

	res := mgr.RollbackWithLedger(context.Background(), nil, nil, nil)
	require.NotNil(t, res)
	assert.Equal(t, "run-99", res.RunID)
	assert.Error(t, res.Error, "a nil plan is still a failure")
}

func TestRollbackResultRunIDEmptyWhenUnset(t *testing.T) {
	// 直接调用 Rollback（不走闭包执行器）时没有 run id：结果依然合法，
	// 只是不可归属，通知适配器必须把空 RunID 当作「无法投递」。
	mgr := NewManager(WithWhitelistAll())

	res := mgr.Rollback(context.Background(), &plan.Plan{}, nil)
	require.NotNil(t, res)
	assert.Empty(t, res.RunID)
}

func TestSetRunIDAppliesToNextRollback(t *testing.T) {
	mgr := NewManager(WithWhitelistAll())
	mgr.SetRunID("run-set-later")

	res := mgr.Rollback(context.Background(), &plan.Plan{}, nil)
	require.NotNil(t, res)
	assert.Equal(t, "run-set-later", res.RunID)
}