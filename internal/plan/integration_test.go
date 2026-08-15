// Package plan 集成测试：端到端覆盖 YAML 解析 → AST 校验 → plan 生成 →
// 批次划分 → 影响面分析 → 哈希锁定。全部使用 mock 目标集（静态 hosts），
// 不依赖真实 SSH/WinRM 通道。
package plan

import (
	"testing"

	"github.com/nexus/levee/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试 fixtures（完整 workflow YAML，覆盖 input/target/batches/step/
// rollback/approval 各块）
// ---------------------------------------------------------------------------

// yamlSerialRollback：serial 批次（strategy 缺省 → generator 视作 serial）+
// 步骤级 rollback + workflow 级 approval + workflow 级 rollback 的完整 fixture。
const yamlSerialRollback = `
name: serial-rollback-demo
version: "1.0"
description: E2E fixture serial batching with rollback and approval
input:
  - name: package_name
    type: string
    required: true
target:
  name: web-tier
  type: ssh
  hosts:
    - host-a
    - host-b
    - host-c
window:
  start: "2024-01-01T00:00:00Z"
  end: "2024-01-01T08:00:00Z"
  timezone: UTC
  max_concurrency: 2
batches:
  max_concurrency: 2
approval:
  level: high
  approvers:
    - alice
    - bob
  min_approvers: 2
steps:
  - name: upgrade-pkg
    action: pkg.upgrade
    args:
      name: "{{ input.package_name }}"
      version: "1.2.3"
    idempotent: true
    rollback:
      strategy: undo-action
      on_failure: abort
      step:
        name: downgrade-pkg
        action: pkg.downgrade
        args:
          name: "{{ input.package_name }}"
  - name: verify-service
    action: svc.restart
    args:
      unit: "{{ input.package_name }}"
    requires_reboot: false
rollback:
  strategy: undo-action
  on_failure: abort
  steps:
    - name: restore-pkg
      action: pkg.restore
      args:
        backup: "/var/backup/pkg"
`

// yamlPercentMultiBatch：percent 批次策略，10 个目标按 [10,50,100] 划分。
const yamlPercentMultiBatch = `
name: percent-multi-batch
version: "1.0"
description: E2E fixture with percent batching across 10 targets
target:
  name: canary-tier
  type: ssh
  hosts:
    - canary-01
    - canary-02
    - canary-03
    - canary-04
    - canary-05
    - canary-06
    - canary-07
    - canary-08
    - canary-09
    - canary-10
batches:
  strategy: percent
  steps: [10, 50, 100]
  max_concurrency: 2
steps:
  - name: deploy
    action: app.deploy
    args:
      artifact: "app.tar.gz"
`

// yamlSingleTarget：单目标单步骤，无 batches 块（strategy 缺省 → serial）。
const yamlSingleTarget = `
name: single-target
version: "1.0"
description: E2E fixture with a single target and single step
target:
  name: solo
  type: ssh
  hosts:
    - lone-host
steps:
  - name: ping
    action: net.ping
    args:
      host: localhost
`

// yamlImpactWithIndirect：含 indirect_targets 声明，用于验证影响面分析。
const yamlImpactWithIndirect = `
name: impact-demo
version: "1.0"
description: E2E fixture exercising direct and indirect impact targets
target:
  name: db-tier
  type: ssh
  hosts:
    - db-master
    - db-replica-1
steps:
  - name: restart-db
    action: db.restart
    args:
      mode: graceful
      indirect_targets:
        - cache-redis-1
        - cache-redis-2
`

// ---------------------------------------------------------------------------
// 辅助函数
// ---------------------------------------------------------------------------

// parseYAML 通过 dsl.Parser 解析 YAML，失败终止测试。
func parseYAML(t *testing.T, yaml string) *dsl.Workflow {
	t.Helper()
	wf, err := dsl.NewParser().ParseBytes([]byte(yaml))
	require.NoError(t, err, "dsl.Parser 解析 YAML 应成功")
	return wf
}

// validateWorkflow 通过 dsl.Validator 校验 AST，失败终止测试。
func validateWorkflow(t *testing.T, wf *dsl.Workflow) {
	t.Helper()
	errs := dsl.NewValidator().Validate(wf)
	require.Empty(t, errs, "dsl.Validator 校验应通过，但发现: %v", errs)
}

// hostsFrom 返回 workflow 第一个 target 的静态 hosts，作为 mock resolved targets。
func hostsFrom(t *testing.T, wf *dsl.Workflow) []string {
	t.Helper()
	require.NotEmpty(t, wf.Targets, "workflow 应至少有一个 target")
	require.NotEmpty(t, wf.Targets[0].Hosts, "第一个 target 应有静态 hosts")
	return wf.Targets[0].Hosts
}

// runPipelineWithHosts 执行完整端到端流程：解析 → 校验 → plan 生成 →
// 影响面分析 → 哈希计算，使用解析出的 hosts 作为 mock resolved targets。
// 返回 plan、影响面报告与哈希供断言使用。
func runPipelineWithHosts(t *testing.T, yaml string) (*Plan, *ImpactReport, string) {
	t.Helper()
	wf := parseYAML(t, yaml)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf)
	p, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err, "plan.Generator.Generate 应成功")
	report := NewImpactAnalyzer().Analyze(p)
	require.NotNil(t, report, "plan.ImpactAnalyzer.Analyze 不应返回 nil")
	hash := ComputeHash(p)
	require.NotEmpty(t, hash, "plan.ComputeHash 不应返回空串")
	return p, report, hash
}

// ---------------------------------------------------------------------------
// 端到端集成测试
// ---------------------------------------------------------------------------

// TestPlanE2E_SerialSingleBatch 验证 serial 批次（strategy 缺省）的完整流程：
// 单批次包含全部目标、步骤编排顺序、步骤级 rollback 透传、max_concurrency
// 透传、影响面标记、哈希可计算且可校验。
func TestPlanE2E_SerialSingleBatch(t *testing.T) {
	wf := parseYAML(t, yamlSerialRollback)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf) // [host-a, host-b, host-c]

	p, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	// Plan 元数据。
	assert.Equal(t, "serial-rollback-demo", p.WorkflowName)
	assert.Equal(t, len(targets), p.TotalTargets)
	assert.NotEmpty(t, p.ID, "plan ID 应已生成")
	assert.False(t, p.CreatedAt.IsZero(), "CreatedAt 应已设置")

	// serial 策略 → 恰好一个批次，包含全部目标。
	require.Len(t, p.Batches, 1, "serial 策略应产生单批次")
	batch := p.Batches[0]
	assert.Equal(t, 0, batch.Index)
	assert.ElementsMatch(t, targets, batch.Targets)
	assert.Equal(t, 2, batch.MaxConcurrency, "max_concurrency 应从 batches 透传")

	// 步骤编排：两步，顺序与 YAML 声明一致。
	require.Len(t, batch.Steps, 2)
	assert.Equal(t, "upgrade-pkg", batch.Steps[0].Name)
	assert.Equal(t, "pkg", batch.Steps[0].Module)
	assert.Equal(t, "upgrade", batch.Steps[0].Action)
	assert.Equal(t, "verify-service", batch.Steps[1].Name)
	assert.Equal(t, "svc", batch.Steps[1].Module)
	assert.Equal(t, "restart", batch.Steps[1].Action)

	// 步骤级 rollback 透传到 PlanStep。
	require.NotNil(t, batch.Steps[0].Rollback, "第一步应携带 rollback")
	assert.Equal(t, "undo-action", batch.Steps[0].Rollback.Strategy)
	assert.Equal(t, "abort", batch.Steps[0].Rollback.OnFailure)
	require.Len(t, batch.Steps[0].Rollback.Steps, 1)
	assert.Equal(t, "downgrade-pkg", batch.Steps[0].Rollback.Steps[0].Name)
	assert.Equal(t, "pkg", batch.Steps[0].Rollback.Steps[0].Module)
	assert.Equal(t, "downgrade", batch.Steps[0].Rollback.Steps[0].Action)

	// 影响面：direct = 全部目标，无 indirect。
	report := NewImpactAnalyzer().Analyze(p)
	require.NotNil(t, report)
	assert.ElementsMatch(t, targets, report.DirectTargets)
	assert.Empty(t, report.IndirectTargets)
	assert.Equal(t, 3, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)

	// 哈希：64 字符 hex（SHA-256），且可自校验。
	hash := ComputeHash(p)
	assert.Len(t, hash, 64)
	assert.True(t, VerifyHash(p, hash))
}

// TestPlanE2E_PercentMultiBatch 验证 percent 批次策略的多批次划分：
// 10 目标按 [10,50,100] → 3 批次，规模 1/4/5，总覆盖无遗漏无重复。
func TestPlanE2E_PercentMultiBatch(t *testing.T) {
	wf := parseYAML(t, yamlPercentMultiBatch)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf) // 10 个 canary

	p, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	assert.Equal(t, 10, p.TotalTargets)

	// percent [10,50,100] on 10 targets → 3 batches: 1, 4, 5.
	require.Len(t, p.Batches, 3, "percent [10,50,100] 应产生 3 个批次")
	assert.Equal(t, 0, p.Batches[0].Index)
	assert.Equal(t, 1, p.Batches[1].Index)
	assert.Equal(t, 2, p.Batches[2].Index)

	assert.Len(t, p.Batches[0].Targets, 1, "第一批 10%% 应为 1 个目标")
	assert.Len(t, p.Batches[1].Targets, 4, "第二批 (50-10)%% 应为 4 个目标")
	assert.Len(t, p.Batches[2].Targets, 5, "第三批 (100-50)%% 应为 5 个目标")

	// 批次总覆盖：每个目标恰好出现一次，无遗漏无重复。
	seen := make(map[string]int)
	for _, b := range p.Batches {
		for _, tgt := range b.Targets {
			seen[tgt]++
		}
	}
	assert.Equal(t, len(targets), len(seen), "批次划分应无重复目标")
	for _, tgt := range targets {
		assert.Equal(t, 1, seen[tgt], "目标 %s 应恰好出现一次", tgt)
	}

	// 每个批次共享同一步骤序列。
	for _, b := range p.Batches {
		require.Len(t, b.Steps, 1)
		assert.Equal(t, "deploy", b.Steps[0].Name)
		assert.Equal(t, "app", b.Steps[0].Module)
		assert.Equal(t, "deploy", b.Steps[0].Action)
		assert.Equal(t, 2, b.MaxConcurrency)
	}

	// 哈希稳定且可校验。
	hash := ComputeHash(p)
	assert.Len(t, hash, 64)
	assert.True(t, VerifyHash(p, hash))
}

// TestPlanE2E_SingleTargetSingleBatch 验证单目标单步骤的边界场景。
func TestPlanE2E_SingleTargetSingleBatch(t *testing.T) {
	p, report, hash := runPipelineWithHosts(t, yamlSingleTarget)

	require.Len(t, p.Batches, 1, "单目标应产生单批次")
	assert.Len(t, p.Batches[0].Targets, 1)
	assert.Equal(t, "lone-host", p.Batches[0].Targets[0])
	assert.Equal(t, 1, p.TotalTargets)

	require.Len(t, p.Batches[0].Steps, 1)
	assert.Equal(t, "ping", p.Batches[0].Steps[0].Name)
	assert.Equal(t, "net", p.Batches[0].Steps[0].Module)
	assert.Equal(t, "ping", p.Batches[0].Steps[0].Action)

	assert.Len(t, report.DirectTargets, 1)
	assert.Empty(t, report.IndirectTargets)
	assert.Equal(t, 1, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)

	assert.Len(t, hash, 64)
	assert.True(t, VerifyHash(p, hash))
}

// TestPlanE2E_ImpactAnalysis_WithIndirect 验证影响面分析正确区分
// direct 与 indirect 目标，并正确计算 TotalAffected 与 RiskLevel。
func TestPlanE2E_ImpactAnalysis_WithIndirect(t *testing.T) {
	p, report, hash := runPipelineWithHosts(t, yamlImpactWithIndirect)

	// direct = 批次内全部目标。
	assert.ElementsMatch(t, []string{"db-master", "db-replica-1"}, report.DirectTargets)
	// indirect = step args 中声明且不在 direct 内的目标。
	assert.ElementsMatch(t, []string{"cache-redis-1", "cache-redis-2"}, report.IndirectTargets)
	assert.Equal(t, 4, report.TotalAffected)
	assert.Equal(t, RiskLevelLow, report.RiskLevel)

	// 哈希可计算且可校验（哈希 canonical 包含影响面，锁定 blast radius）。
	assert.Len(t, hash, 64)
	assert.True(t, VerifyHash(p, hash))
}

// TestPlanE2E_HashDeterminism 验证哈希确定性：相同 workflow + 相同 resolved
// targets 两次生成的 plan，即使 ID/CreatedAt 不同，哈希必须相同。
func TestPlanE2E_HashDeterminism(t *testing.T) {
	wf := parseYAML(t, yamlSerialRollback)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf)

	p1, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)
	p2, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	h1 := ComputeHash(p1)
	h2 := ComputeHash(p2)
	assert.Equal(t, h1, h2, "相同输入应产生确定性哈希")
	assert.Len(t, h1, 64)

	// 哈希确定性来自 canonical 排除了 ID/CreatedAt（hash.go 的 canonicalPlan
	// 不含这两个字段）。此处不断言 ID/CreatedAt 必然不同：两次 time.Now 在
	// 同纳秒内可能返回相等值（系统时钟精度有限），crypto.rand 理论上也可
	// 碰撞；即便它们恰好相同，哈希相等这一核心断言依然成立。
	_ = p1.ID
	_ = p2.ID
	_ = p1.CreatedAt
	_ = p2.CreatedAt
}

// TestPlanE2E_HashTargetOrderInvariant_Serial 验证 serial 策略下目标顺序
// 不影响哈希：canonical 对批次内目标排序，故打乱输入顺序哈希不变。
func TestPlanE2E_HashTargetOrderInvariant_Serial(t *testing.T) {
	wf := parseYAML(t, yamlSerialRollback)
	validateWorkflow(t, wf)
	ordered := hostsFrom(t, wf) // [host-a, host-b, host-c]

	// 打乱顺序。
	shuffled := []string{ordered[2], ordered[0], ordered[1]} // [host-c, host-a, host-b]

	p1, err := NewGenerator().Generate(wf, ordered)
	require.NoError(t, err)
	p2, err := NewGenerator().Generate(wf, shuffled)
	require.NoError(t, err)

	// serial 把全部目标放入同一批次；canonical 对批次内目标排序，
	// 因此不同输入顺序应产生相同哈希。
	assert.Equal(t, ComputeHash(p1), ComputeHash(p2),
		"serial 策略下目标顺序不应影响哈希")
}

// TestPlanE2E_HashContentSensitive 验证哈希对 step args 敏感：
// 改变 args 应导致哈希变化。
func TestPlanE2E_HashContentSensitive(t *testing.T) {
	wf := parseYAML(t, yamlSingleTarget)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf)

	p1, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	// 改变 step args，哈希应变化。
	wf2 := parseYAML(t, yamlSingleTarget)
	validateWorkflow(t, wf2)
	wf2.Steps[0].Args["host"] = "remote-host" // 原为 localhost
	p2, err := NewGenerator().Generate(wf2, targets)
	require.NoError(t, err)

	assert.NotEqual(t, ComputeHash(p1), ComputeHash(p2),
		"step args 不同应导致哈希不同")
}

// TestPlanE2E_HashWorkflowNameSensitive 验证哈希对 workflow name 敏感。
func TestPlanE2E_HashWorkflowNameSensitive(t *testing.T) {
	wf := parseYAML(t, yamlSingleTarget)
	validateWorkflow(t, wf)
	targets := hostsFrom(t, wf)

	p1, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	wf2 := parseYAML(t, yamlSingleTarget)
	wf2.Meta.Name = "renamed-workflow"
	p2, err := NewGenerator().Generate(wf2, targets)
	require.NoError(t, err)

	assert.NotEqual(t, ComputeHash(p1), ComputeHash(p2),
		"workflow name 不同应导致哈希不同")
}

// TestPlanE2E_EmptyTargetsError 验证空 resolved targets 被拒绝。
func TestPlanE2E_EmptyTargetsError(t *testing.T) {
	wf := parseYAML(t, yamlSingleTarget)
	validateWorkflow(t, wf)

	p, err := NewGenerator().Generate(wf, []string{})
	require.Error(t, err, "空 resolved targets 应报错")
	assert.Nil(t, p)
}

// TestPlanE2E_NilWorkflowError 验证 nil workflow 被拒绝。
func TestPlanE2E_NilWorkflowError(t *testing.T) {
	p, err := NewGenerator().Generate(nil, []string{"host-a"})
	require.Error(t, err, "nil workflow 应报错")
	assert.Nil(t, p)
}

// TestPlanE2E_FixedStrategy_FromAST 验证 fixed 批次策略的划分。
//
// 注意：dsl.Parser 的内置 validate 不接受 "fixed" 策略（仅认 percent/
// one-per-target/count/by-tag/by-group），但 plan.Generator 支持 fixed。
// 此处手动构造 AST，走 dsl.Validator + plan.Generator 路径覆盖 fixed 划分，
// 并验证 leftover 目标进入尾部批次。
func TestPlanE2E_FixedStrategy_FromAST(t *testing.T) {
	targets := []string{"h1", "h2", "h3", "h4", "h5", "h6"}
	wf := &dsl.Workflow{
		Meta:    dsl.WorkflowMeta{Name: "fixed-demo", Version: "1.0"},
		Targets: []dsl.TargetGroup{{Name: "t", Type: "ssh", Hosts: targets}},
		Batches: dsl.BatchConfig{Strategy: "fixed", Steps: []int{2, 3}, MaxConcurrency: 2},
		Steps: []dsl.Step{{
			Name:   "deploy",
			Module: "app",
			Action: "deploy",
			Args:   map[string]any{"artifact": "app.tar.gz"},
		}},
	}

	// dsl.Validator 应通过（fixed + steps>0 合法）。
	errs := dsl.NewValidator().Validate(wf)
	require.Empty(t, errs, "Validator 应接受 fixed 策略: %v", errs)

	p, err := NewGenerator().Generate(wf, targets)
	require.NoError(t, err)

	// fixed [2,3] on 6 targets → batch0=[h1,h2], batch1=[h3,h4,h5], leftover=[h6].
	require.Len(t, p.Batches, 3, "fixed [2,3] on 6 targets 应产生 3 批次（含 leftover）")
	assert.Len(t, p.Batches[0].Targets, 2)
	assert.Len(t, p.Batches[1].Targets, 3)
	assert.Len(t, p.Batches[2].Targets, 1, "leftover 目标应进入尾部批次")

	// 批次索引连续，max_concurrency 透传。
	for i, b := range p.Batches {
		assert.Equal(t, i, b.Index, "批次索引应连续")
		assert.Equal(t, 2, b.MaxConcurrency)
	}

	// 总覆盖无遗漏。
	seen := make(map[string]int)
	for _, b := range p.Batches {
		for _, tgt := range b.Targets {
			seen[tgt]++
		}
	}
	assert.Equal(t, len(targets), len(seen))
	for _, tgt := range targets {
		assert.Equal(t, 1, seen[tgt], "目标 %s 应恰好出现一次", tgt)
	}

	// 哈希可计算且可校验。
	hash := ComputeHash(p)
	assert.Len(t, hash, 64)
	assert.True(t, VerifyHash(p, hash))
}

// TestPlanE2E_VerifyHash_EdgeCases 验证 ComputeHash/VerifyHash 的边界行为。
func TestPlanE2E_VerifyHash_EdgeCases(t *testing.T) {
	// nil plan → 空哈希，VerifyHash false。
	assert.Empty(t, ComputeHash(nil))
	assert.False(t, VerifyHash(nil, "deadbeef"))

	// 正常 plan + 空期望 → false；错误期望 → false。
	p, _, hash := runPipelineWithHosts(t, yamlSingleTarget)
	assert.False(t, VerifyHash(p, ""), "空期望哈希应校验失败")
	assert.False(t, VerifyHash(p, "0000000000000000000000000000000000000000000000000000000000000000"),
		"错误期望哈希应校验失败")
	assert.True(t, VerifyHash(p, hash), "正确期望哈希应校验成功")
}
