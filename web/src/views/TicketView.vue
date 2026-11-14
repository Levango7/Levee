<script setup lang="ts">
/**
 * DB Change Ticket — 全链视图
 *
 * 单一数据源：GET /api/v1/changes/:id/ticket
 * 后端已聚合：变更信息、SQL 与审核、回滚方案、变更窗口、审批链、执行证据、审计验证
 * 前端只负责“把后端给的结构按人类可读方式分区渲染”，不做二次计算、不拼接多接口。
 *
 * 设计原则：
 * - 诚实三态：planState=unreadable 时不显示 SQL 步骤，给出“计划存在但不可读”的提示
 * - 审批三态：legacyApproval / staleApproval / matchesPlan 三标记全透出，操作员一眼看清审批绑定关系
 * - 审计三态：verified / unverifiable / verifyError，校验出错不伪装成“无记录”
 * - 空态友好：每个区块都有“无数据”占位，避免留白让人困惑
 */
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElTag, ElCard, ElDivider, ElSpace, ElEmpty, ElTable, ElTableColumn, ElTooltip, ElAlert } from 'element-plus'

import { changesApi } from '@/api'
import type { DBChangeTicket, PlanState, ApprovalStatus, ExecutionVerdict, RollbackStrategy } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import StatusTag from '@/components/StatusTag.vue'
import { approvalStatusLabel, executionVerdictLabel, rollbackStrategyLabel, planStateLabel } from '@/utils/format'
import { classifyError, errorMessage, type LoadTier } from '@/utils/apiState'

const route = useRoute()

const changeId = computed(() => String(route.params.id || ''))
const loading = ref(false)
const ticket = ref<DBChangeTicket | null>(null)
const error = ref<unknown>(null)

const loadTier = computed<LoadTier>(() =>
  error.value === null ? 'ok' : classifyError(error.value),
)
const loadText = computed(() => errorMessage(error.value, '加载发布单失败'))

async function load(): Promise<void> {
  if (!changeId.value) return
  loading.value = true
  error.value = null
  try {
    ticket.value = await changesApi.ticket(changeId.value)
  } catch (err) {
    error.value = err
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ===== 格式化工具 =====

/** 计划三态标签 & 类型 */
function planStateInfo(state: PlanState) {
  const label = planStateLabel(state)
  const typeMap: Record<PlanState, 'success' | 'warning' | 'info' | 'danger'> = {
    none: 'info',
    readable: 'success',
    unreadable: 'danger',
  }
  return { label, type: typeMap[state] }
}

/** 审批状态标签类型 */
function approvalStatusType(status: ApprovalStatus): 'success' | 'danger' | 'info' | 'warning' {
  const map: Record<ApprovalStatus, 'success' | 'danger' | 'info' | 'warning'> = {
    pending: 'warning',
    approved: 'success',
    rejected: 'danger',
    expired: 'info',
  }
  return map[status]
}

/** 执行裁决标签类型 — evidence.Kind vocabulary */
function executionVerdictType(verdict: ExecutionVerdict): 'success' | 'danger' | 'warning' | 'info' {
  const map: Record<ExecutionVerdict, 'success' | 'danger' | 'warning' | 'info'> = {
    not_started: 'info',
    in_flight: 'warning',
    complete: 'success',
    partial: 'warning',
    missing: 'danger',
    unrecorded: 'info',
  }
  return map[verdict]
}

/** 回滚策略标签类型 — dsl.RollbackStrategy vocabulary */
function rollbackStrategyType(strategy: RollbackStrategy): 'success' | 'warning' | 'info' {
  const map: Record<RollbackStrategy, 'success' | 'warning' | 'info'> = {
    snapshot: 'success',
    'undo-action': 'warning',
    'config-revert': 'info',
  }
  return map[strategy]
}

/** SQL 审核严重级 -> ElTag 类型 (pass/warn/fail -> success/warning/danger) */
function severityType(sev: 'pass' | 'warn' | 'fail'): 'success' | 'warning' | 'danger' {
  return sev === 'pass' ? 'success' : sev === 'warn' ? 'warning' : 'danger'
}

function formatDuration(ms: number) {
  if (ms < 1000) return `${ms} ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)} s`
  return `${(ms / 60000).toFixed(1)} min`
}

// ===== 计算属性：各区块是否有内容 =====

const hasSql = computed(() => ticket.value?.sql?.hasSql ?? false)
const hasReview = computed(() => !!ticket.value?.sql?.review)
const hasRollback = computed(() => ticket.value?.rollback?.hasUndo ?? false)
const hasApproval = computed(() => ticket.value?.approval?.hasApproval ?? false)
const hasExecution = computed(() => ticket.value?.execution?.hasEvidence ?? false)
const hasAuditTraces = computed(() => (ticket.value?.audit?.traces?.length ?? 0) > 0)

const planStateTag = computed(() => ticket.value ? planStateInfo(ticket.value.change.planState) : null)
const approvalStatusTag = computed(() => ticket.value ? approvalStatusType(ticket.value.approval.status) : null)
const executionVerdictTag = computed(() => ticket.value ? executionVerdictType(ticket.value.execution.verdict) : null)
const rollbackStrategyTag = computed(() => ticket.value ? rollbackStrategyType(ticket.value.rollback.strategy) : null)

const decisions = computed(() => ticket.value?.approval?.decisions ?? [])
const decidedCount = computed(() => decisions.value.filter(d => d.action).length)
const pendingCount = computed(() => ticket.value?.approval?.approvers?.length
  ? ticket.value.approval.approvers.length - decidedCount.value
  : 0)

// activeTab for ElTabs
const activeTab = ref('sql')

// Safe getters for computed values that can be null
const safePlanStateTag = computed(() => planStateTag.value ?? { label: '—', type: 'info' as const })
const safeApprovalStatusTag = computed(() => approvalStatusTag.value ?? 'info')
const safeExecutionVerdictTag = computed(() => executionVerdictTag.value ?? 'info')
const safeRollbackStrategyTag = computed(() => rollbackStrategyTag.value ?? 'info')

// Helper to safely get ticket (for template)
const t = computed((): DBChangeTicket | null => ticket.value)

// Local timestamp formatters - inline dayjs to avoid template type inference issues
import dayjs from 'dayjs'
function fmtTs(ts: string | number | undefined | null): string {
  if (!ts) return '-'
  const n = typeof ts === 'string' ? parseInt(ts, 10) : ts
  if (isNaN(n)) return '-'
  return dayjs.unix(n).format('YYYY-MM-DD HH:mm:ss')
}
const fmtCreatedAt = computed<string>(() => {
  const tk = t.value
  return tk?.change.createdAt ? fmtTs(tk.change.createdAt) : '-'
})
const fmtUpdatedAt = computed<string>(() => {
  const tk = t.value
  return tk?.change.updatedAt ? fmtTs(tk.change.updatedAt) : '-'
})
const fmtReviewedAt = computed<string>(() => {
  const tk = t.value
  return tk?.sql.review?.reviewedAt ? fmtTs(tk.sql.review.reviewedAt) : '-'
})
const fmtEnforcedAt = computed<string>(() => {
  const tk = t.value
  return tk?.window.enforcedAt ? fmtTs(tk.window.enforcedAt) : '-'
})
const fmtExpiresAt = computed<string>(() => {
  const tk = t.value
  return tk?.approval.expiresAt ? fmtTs(tk.approval.expiresAt) : '-'
})
const fmtDecidedAt = (ts: string | number | undefined): string => ts ? fmtTs(ts) : '-'
const fmtStartedAt = (ts: string | number | undefined): string => ts ? fmtTs(ts) : '-'
const fmtFinishedAt = (ts: string | number | undefined): string => ts ? fmtTs(ts) : '-'
const fmtTraceTimestamp = (ts: string | number | undefined): string => ts ? fmtTs(ts) : '-'
</script>

<template>
  <div class="ticket-view">
    <PageHeader
      :title="t ? t.change.label : '变更发布单'"
      :subtitle="t ? `ID: ${t.change.id} · ${t.change.environment}` : ''"
      :loading="loading"
      :error="loadText"
      :tier="loadTier"
      @retry="load"
      back-link="/changes"
      back-label="返回变更看板"
    />

    <div v-if="!loading && !t" class="empty-state">
      <ElEmpty description="未找到该发布单" />
    </div>

    <div v-else-if="t" class="ticket-content">
      <!-- 顶部摘要卡片 -->
      <ElCard class="summary-card">
        <template #header>
          <div class="summary-header">
            <h3>发布单摘要</h3>
            <ElSpace>
              <ElTag :type="safePlanStateTag.type" effect="light">{{ safePlanStateTag.label }}</ElTag>
              <ElTag v-if="hasApproval" :type="safeApprovalStatusTag" effect="light">{{ approvalStatusLabel(t.approval.status) }}</ElTag>
              <ElTag :type="safeExecutionVerdictTag" effect="light">{{ executionVerdictLabel(t.execution.verdict) }}</ElTag>
              <ElTag :type="safeRollbackStrategyTag" effect="light">{{ rollbackStrategyLabel(t.rollback.strategy) }}</ElTag>
            </ElSpace>
          </div>
        </template>

        <ElDescriptions border :column="4" size="small">
          <ElDescriptionsItem label="变更 ID" :span="2">{{ t.change.id }}</ElDescriptionsItem>
          <ElDescriptionsItem label="环境" :span="1">{{ t.change.environment }}</ElDescriptionsItem>
          <ElDescriptionsItem label="优先级" :span="1">
            {{ t.change.priority }}
          </ElDescriptionsItem>
          <ElDescriptionsItem label="状态" :span="1">
            <StatusTag :status="t.change.status" />
          </ElDescriptionsItem>
          <ElDescriptionsItem label="创建者" :span="1">{{ t.change.creator }}</ElDescriptionsItem>
          <ElDescriptionsItem label="工作流" :span="1">{{ t.change.workflowFile }}</ElDescriptionsItem>
          <ElDescriptionsItem label="模板" :span="1">{{ t.change.templateName }}</ElDescriptionsItem>
          <ElDescriptionsItem label="计划哈希" :span="2" class="monospace">{{ t.change.planHash || '—' }}</ElDescriptionsItem>
          <ElDescriptionsItem label="创建时间" :span="1">{{ fmtCreatedAt }}</ElDescriptionsItem>
          <ElDescriptionsItem label="更新时间" :span="1">{{ fmtUpdatedAt }}</ElDescriptionsItem>
        </ElDescriptions>
      </ElCard>

      <ElDivider />

      <!-- 六大区块 Tab -->
      <ElTabs v-model="activeTab" class="ticket-tabs">
        <!-- 1. SQL 与审核 -->
        <ElTabPane label="SQL 与审核" name="sql" :disabled="!hasSql && !hasReview">
          <div v-if="t.change.planState === 'unreadable'" class="alert-section">
            <ElAlert
              title="计划不可读"
              :description="`该变更已生成计划（plan_hash: ${t.change.planHash}），但解析失败。SQL 步骤、回滚动作、审核结论均无法从计划中提取。请检查计划生成日志或联系平台管理员。`"
              type="error"
              show-icon
              closable
            />
          </div>

          <div v-else-if="t.change.planState === 'none'" class="alert-section">
            <ElAlert
              title="暂无计划"
              description="该变更尚未生成计划（或处于草稿态），SQL 步骤与审核结论将在计划生成后出现。"
              type="info"
              show-icon
              closable
            />
          </div>

          <div v-else class="sql-section">
            <!-- SQL 审核汇总 -->
            <ElCard v-if="hasReview" class="review-card">
              <template #header>
                <div class="card-header">
                  <span>SQL 审核汇总</span>
                  <ElTag :type="severityType(t.sql.review!.verdict)" effect="dark">{{ t.sql.review!.verdict.toUpperCase() }}</ElTag>
                  <ElTag type="info" effect="light">评分 {{ t.sql.review!.score }}</ElTag>
                </div>
              </template>
              <ElDescriptions border :column="4" size="small">
                <ElDescriptionsItem label="审核人" :span="1">{{ t.sql.review!.reviewer }}</ElDescriptionsItem>
                <ElDescriptionsItem label="审核时间" :span="3">{{ fmtReviewedAt }}</ElDescriptionsItem>
              </ElDescriptions>

              <ElTable :data="t.sql.review!.findings" border size="small" stripe>
                <ElTableColumn prop="ruleId" label="规则 ID" width="120" />
                <ElTableColumn prop="ruleName" label="规则名称" width="180" />
                <ElTableColumn prop="severity" label="严重级" width="100">
                  <template #default="scope">
                    <ElTag :type="severityType(scope.row.severity)" effect="light">{{ scope.row.severity }}</ElTag>
                  </template>
                </ElTableColumn>
                <ElTableColumn prop="message" label="说明" min-width="200" />
                <ElTableColumn prop="line" label="行" width="80" />
                <ElTableColumn prop="sqlSnippet" label="SQL 片段" min-width="200">
                  <template #default="scope">
                    <code class="sql-snippet" v-if="scope.row.sqlSnippet">{{ scope.row.sqlSnippet }}</code>
                    <span v-else class="text-muted">—</span>
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>

            <!-- SQL 步骤 -->
            <ElCard v-if="hasSql && t.sql.steps.length > 0" class="steps-card">
              <template #header>
                <span>SQL 步骤 (共 {{ t.sql.steps.length }} 条)</span>
              </template>
              <ElTable :data="t.sql.steps" border size="small" stripe>
                <ElTableColumn prop="step" label="步骤名" width="180" />
                <ElTableColumn prop="batchNo" label="批次" width="80" />
                <ElTableColumn prop="hostCount" label="目标主机数" width="120" />
                <ElTableColumn prop="sql" label="SQL" min-width="400">
                  <template #default="scope">
                    <code class="sql-cell">{{ scope.row.sql }}</code>
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>

            <ElEmpty v-if="hasSql && t.sql.steps.length === 0" description="计划中无 SQL 步骤" />
          </div>
        </ElTabPane>

        <!-- 2. 回滚方案 -->
        <ElTabPane label="回滚方案" name="rollback" :disabled="!hasRollback">
          <div v-if="(t.rollback.strategy as string) === 'none'" class="alert-section">
            <ElAlert
              title="无回滚方案"
              description="该变更未声明回滚策略（strategy=none）。执行失败时将无法自动补偿，需人工介入。"
              type="warning"
              show-icon
              closable
            />
          </div>

          <div v-else class="rollback-section">
            <ElCard class="strategy-card">
              <template #header>
                <div class="card-header">
                  <span>回滚策略</span>
                  <ElTag :type="safeRollbackStrategyTag" effect="dark">{{ rollbackStrategyLabel(t.rollback.strategy) }}</ElTag>
                </div>
              </template>
              <ElDescriptions border :column="4" size="small">
                <ElDescriptionsItem label="失败时行为" :span="2">{{ t.rollback.onFailure }}</ElDescriptionsItem>
                <ElDescriptionsItem label="执行后验证" :span="1">{{ t.rollback.verifyAfter ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="快照路径数" :span="1">{{ t.rollback.snapshotPaths.length }}</ElDescriptionsItem>
              </ElDescriptions>

              <ElDivider v-if="t.rollback.snapshotPaths.length > 0" />
              <div v-if="t.rollback.snapshotPaths.length > 0" class="snapshot-paths">
                <h4>快照路径</h4>
                <ul>
                  <li v-for="p in t.rollback.snapshotPaths" :key="p" class="monospace">{{ p }}</li>
                </ul>
              </div>
            </ElCard>

            <ElCard v-if="hasRollback && t.rollback.stepUndos.length > 0" class="undos-card">
              <template #header>
                <span>步骤级回滚动作 (共 {{ t.rollback.stepUndos.length }} 个)</span>
              </template>
              <ElTable :data="t.rollback.stepUndos" border size="small" stripe>
                <ElTableColumn prop="stepName" label="步骤名" width="180" />
                <ElTableColumn prop="action" label="动作" width="150" />
                <ElTableColumn prop="params" label="参数" min-width="300">
                  <template #default="scope">
                    <pre class="params-pre">{{ JSON.stringify(scope.row.params, null, 2) }}</pre>
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>

            <ElEmpty v-if="!hasRollback" description="无步骤级回滚动作" />
          </div>
        </ElTabPane>

        <!-- 3. 变更窗口 -->
        <ElTabPane label="变更窗口" name="window">
          <ElCard>
            <ElDescriptions border :column="4" size="small">
              <ElDescriptionsItem label="强制执行时间" :span="2">
                {{ t.window.enforcedAt ? fmtEnforcedAt : '—' }}
              </ElDescriptionsItem>
              <ElDescriptionsItem label="按运行持久化" :span="1">{{ t.window.persistedPerRun ? '是' : '否' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="备注" :span="3">{{ t.window.note || '—' }}</ElDescriptionsItem>
            </ElDescriptions>
          </ElCard>
        </ElTabPane>

        <!-- 4. 审批链 -->
        <ElTabPane label="审批链" name="approval" :disabled="!hasApproval">
          <div v-if="!hasApproval" class="alert-section">
            <ElAlert
              title="无审批链"
              description="该变更未创建审批链（或处于不需要审批的优先级）。"
              type="info"
              show-icon
              closable
            />
          </div>

          <div v-else class="approval-section">
            <ElCard class="approval-meta-card">
              <template #header>
                <div class="card-header">
                  <span>审批元信息</span>
                  <ElTag :type="safeApprovalStatusTag" effect="dark">{{ approvalStatusLabel(t.approval.status) }}</ElTag>
                  <ElTag type="info" effect="light">{{ t.approval.level }}</ElTag>
                </div>
              </template>
              <ElDescriptions border :column="4" size="small">
                <ElDescriptionsItem label="审批人" :span="2">{{ t.approval.approvers.join(', ') || '—' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="最小通过票数" :span="1">{{ t.approval.minApprovers }}</ElDescriptionsItem>
                <ElDescriptionsItem label="已决/待决" :span="1">{{ `${decidedCount} / ${pendingCount}` }}</ElDescriptionsItem>
                <ElDescriptionsItem label="绑定当前计划" :span="1">
                  <ElTag :type="t.approval.matchesPlan ? 'success' : 'danger'" effect="light">
                    {{ t.approval.matchesPlan ? '是' : '否' }}
                  </ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="Legacy 审批" :span="1">
                  <ElTag v-if="t.approval.legacyApproval" type="warning" effect="light">是（无 plan_hash）</ElTag>
                  <ElTag v-else type="success" effect="light">否</ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="过期计划审批" :span="1">
                  <ElTag v-if="t.approval.staleApproval" type="warning" effect="light">是（绑定旧计划）</ElTag>
                  <ElTag v-else type="success" effect="light">否</ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="发起人" :span="1">{{ t.approval.initiator }}</ElDescriptionsItem>
                <ElDescriptionsItem label="排除发起人" :span="1">{{ t.approval.excludeInitiator ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="过期时间" :span="1">{{ t.approval.expiresAt ? fmtExpiresAt : '—' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="审批 plan_hash" :span="3" class="monospace">{{ t.approval.planHash || '—' }}</ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>

            <ElCard v-if="decisions.length > 0" class="approval-decisions-card">
              <template #header>
                <span>审批决策记录 (共 {{ decisions.length }} 条)</span>
              </template>
              <ElTable :data="decisions" border size="small" stripe>
                <ElTableColumn prop="approver" label="审批人" width="150" />
                <ElTableColumn prop="action" label="动作" width="100">
                  <template #default="scope">
                    <ElTag :type="scope.row.action === 'approve' ? 'success' : 'danger'" effect="light">{{ scope.row.action }}</ElTag>
                  </template>
                </ElTableColumn>
                <ElTableColumn prop="reason" label="理由" min-width="200" />
                <ElTableColumn prop="decidedAt" label="决策时间" width="180">
                  <template #default="scope">
                    {{ fmtDecidedAt(scope.row.decidedAt) }}
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>

            <ElEmpty v-if="decisions.length === 0" description="暂无审批决策" />
          </div>
        </ElTabPane>

        <!-- 5. 执行证据 -->
        <ElTabPane label="执行证据" name="execution" :disabled="!hasExecution">
          <div v-if="!hasExecution" class="alert-section">
            <ElAlert
              title="无执行证据"
              description="该变更尚未执行，或执行未产生证据（engine 未启用 / 仅状态推进）。"
              type="info"
              show-icon
              closable
            />
          </div>

          <div v-else class="execution-section">
            <ElCard class="verdict-card">
              <template #header>
                <div class="card-header">
                  <span>执行裁决</span>
                  <ElTag :type="safeExecutionVerdictTag" effect="dark">{{ executionVerdictLabel(t.execution.verdict) }}</ElTag>
                </div>
              </template>
              <ElDescriptions border :column="4" size="small">
                <ElDescriptionsItem label="证据条数" :span="1">{{ t.execution.evidence.length }}</ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>

            <ElCard class="evidence-card">
              <template #header>
                <span>逐主机证据 (共 {{ t.execution.evidence.length }} 条)</span>
              </template>
              <ElTable :data="t.execution.evidence" border size="small" stripe>
                <ElTableColumn prop="host" label="主机" width="150" />
                <ElTableColumn prop="batchNo" label="批次" width="80" />
                <ElTableColumn prop="stepName" label="步骤" width="150" />
                <ElTableColumn prop="status" label="状态" width="100">
                  <template #default="scope">
                    <ElTag :type="scope.row.status === 'success' ? 'success' : scope.row.status === 'failed' ? 'danger' : 'warning'" effect="light">{{ scope.row.status }}</ElTag>
                  </template>
                </ElTableColumn>
                <ElTableColumn prop="startedAt" label="开始时间" width="180">
                  <template #default="scope">{{ fmtStartedAt(scope.row.startedAt) }}</template>
                </ElTableColumn>
                <ElTableColumn prop="finishedAt" label="结束时间" width="180">
                  <template #default="scope">{{ fmtFinishedAt(scope.row.finishedAt) }}</template>
                </ElTableColumn>
                <ElTableColumn prop="duration" label="耗时" width="100">
                  <template #default="scope">
                    {{ formatDuration(Number(scope.row.finishedAt) - Number(scope.row.startedAt)) }}
                  </template>
                </ElTableColumn>
                <ElTableColumn prop="exitCode" label="退出码" width="100" />
                <ElTableColumn prop="output" label="输出" min-width="300">
                  <template #default="scope">
                    <ElTooltip :content="scope.row.output" placement="top">
                      <code class="output-cell">{{ scope.row.output.length > 80 ? scope.row.output.slice(0, 80) + '…' : scope.row.output }}</code>
                    </ElTooltip>
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>
          </div>
        </ElTabPane>

        <!-- 6. 审计验证 -->
        <ElTabPane label="审计验证" name="audit">
          <ElCard class="audit-verdict-card">
            <template #header>
              <div class="card-header">
                <span>哈希链验证</span>
                <ElTag v-if="t.audit.verified" type="success" effect="dark">验证通过</ElTag>
                <ElTag v-else-if="t.audit.unverifiable" type="warning" effect="dark">不可验证</ElTag>
                <ElTag v-else-if="t.audit.verifyError" type="danger" effect="dark">校验出错</ElTag>
                <ElTag v-else type="info" effect="dark">未知</ElTag>
              </div>
            </template>
            <ElDescriptions border :column="4" size="small">
              <ElDescriptionsItem label="验证通过" :span="1">{{ t.audit.verified ? '是' : '否' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="不可验证" :span="1">{{ t.audit.unverifiable ? '是' : '否' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="校验错误" :span="2" v-if="t.audit.verifyError">{{ t.audit.verifyError }}</ElDescriptionsItem>
            </ElDescriptions>
          </ElCard>

          <ElCard v-if="hasAuditTraces" class="audit-traces-card">
            <template #header>
              <span>审计轨迹 (共 {{ t.audit.traces.length }} 条)</span>
            </template>
            <ElTable :data="t.audit.traces" border size="small" stripe>
              <ElTableColumn prop="timestamp" label="时间" width="180">
                <template #default="scope">{{ fmtTraceTimestamp(scope.row.timestamp) }}</template>
              </ElTableColumn>
              <ElTableColumn prop="action" label="动作" width="150" />
              <ElTableColumn prop="targetHost" label="目标主机" width="150" />
              <ElTableColumn prop="actor" label="操作者" width="120" />
              <ElTableColumn prop="detail" label="详情" min-width="200" />
              <ElTableColumn prop="currHash" label="当前哈希" width="200" class="monospace" />
            </ElTable>
          </ElCard>

          <ElEmpty v-if="!hasAuditTraces" description="无审计轨迹" />
        </ElTabPane>
      </ElTabs>
    </div>
  </div>
</template>

<style scoped>
.ticket-view {
  padding: 16px 24px 24px;
  max-width: 1400px;
  margin: 0 auto;
}

.summary-card {
  margin-bottom: 8px;
}

.summary-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
  width: 100%;
}

.summary-header h3 {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
}

.ticket-tabs :deep(.el-tabs__content) {
  padding-top: 16px;
}

.alert-section {
  padding: 16px;
}

.sql-section,
.rollback-section,
.approval-section,
.execution-section {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.card-header {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}

.review-card,
.steps-card,
.strategy-card,
.undos-card,
.approval-meta-card,
.approval-decisions-card,
.verdict-card,
.evidence-card,
.audit-verdict-card,
.audit-traces-card {
  width: 100%;
}

.monospace {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 12px;
  background: var(--el-fill-color-light);
  padding: 2px 6px;
  border-radius: 4px;
}

.sql-snippet,
.sql-cell {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
}

.sql-cell {
  max-height: 60px;
  overflow: hidden;
  display: block;
}

.output-cell {
  font-family: 'JetBrains Mono', 'Fira Code', monospace;
  font-size: 12px;
  white-space: pre-wrap;
  word-break: break-all;
  max-width: 400px;
  display: block;
}

.params-pre {
  margin: 0;
  font-size: 11px;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 120px;
  overflow: auto;
}

.snapshot-paths ul {
  margin: 8px 0 0;
  padding-left: 20px;
}

.snapshot-paths li {
  margin: 4px 0;
  word-break: break-all;
}

.text-muted {
  color: var(--el-text-color-placeholder);
}

.empty-state {
  display: flex;
  justify-content: center;
  align-items: center;
  min-height: 300px;
}
</style>
