<script setup lang="ts">
/**
 * DB Change Ticket — 全链视图
 *
 * 单一数据源：GET /api/v1/changes/:id/ticket
 * 后端已聚合：变更信息、SQL 与审核、回滚方案、变更窗口、审批链、执行证据、审计验证
 * 前端只负责「把后端给的结构按人类可读方式分区渲染」，不做二次计算、不拼接多接口。
 *
 * 设计原则：
 * - 诚实三态：planState=unreadable 时不显示 SQL 步骤，给出「计划存在但不可读」的提示
 * - 审批三态：legacyApproval / staleApproval / matchesPlan 三标记全透出，操作员一眼看清审批绑定关系
 * - 审计三态：verified / unverifiable / verifyError，校验出错不伪装成「无记录」
 * - 空态友好：每个区块都有「无数据」占位，避免留白让人困惑
 *
 * 字段名一律照 wire（protojson / proto/levee.proto 的 DBChangeTicket）：
 * 早期版本曾把 workflow/template 写成 workflowFile/templateName、把 SQL 步骤
 * 多写两个 proto 里没有的字段，页面上全是空白。现在这份 DTO 由
 * web/src/api/__contract__/wire.json 的「dbchange-ticket」条目钉住。
 */
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  ElTabs, ElTabPane, ElDescriptions, ElDescriptionsItem, ElTag, ElCard, ElSpace,
  ElEmpty, ElTable, ElTableColumn, ElAlert,
} from 'element-plus'

import { changesApi } from '@/api'
import type { DBChangeTicket, PlanState, ApprovalStatus, ExecutionVerdict, RollbackStrategy } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import StatusTag from '@/components/StatusTag.vue'
import StateBlock from '@/components/StateBlock.vue'
import {
  approvalStatusLabel, executionVerdictLabel, rollbackStrategyLabel, planStateLabel,
  priorityLabel, formatTimestamp,
} from '@/utils/format'
import { classifyError, errorMessage, type LoadTier } from '@/utils/apiState'

const route = useRoute()
const router = useRouter()

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
    ticket.value = null
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ---------------------------------------------------------------------------
// 标签与展示辅助
// ---------------------------------------------------------------------------

function planStateInfo(state: PlanState): { label: string; type: 'success' | 'warning' | 'info' | 'danger' } {
  const typeMap: Record<PlanState, 'success' | 'warning' | 'info' | 'danger'> = {
    none: 'info',
    readable: 'success',
    unreadable: 'danger',
  }
  return { label: planStateLabel(state), type: typeMap[state] ?? 'info' }
}

function approvalStatusType(status: ApprovalStatus): 'success' | 'danger' | 'info' | 'warning' {
  const map: Record<ApprovalStatus, 'success' | 'danger' | 'info' | 'warning'> = {
    pending: 'warning',
    approved: 'success',
    rejected: 'danger',
    expired: 'info',
  }
  return map[status] ?? 'info'
}

function executionVerdictType(verdict: ExecutionVerdict): 'success' | 'danger' | 'warning' | 'info' {
  const map: Record<ExecutionVerdict, 'success' | 'danger' | 'warning' | 'info'> = {
    not_started: 'info',
    in_flight: 'warning',
    complete: 'success',
    partial: 'warning',
    missing: 'danger',
    unrecorded: 'info',
  }
  return map[verdict] ?? 'info'
}

function rollbackStrategyType(strategy: RollbackStrategy): 'success' | 'warning' | 'info' {
  const map: Record<RollbackStrategy, 'success' | 'warning' | 'info'> = {
    snapshot: 'success',
    'undo-action': 'warning',
    'config-revert': 'info',
  }
  return map[strategy] ?? 'info'
}

// 页面上的可选值都在 computed 里兜底：分段缺字段时显示「—」，不报错。
const t = computed(() => ticket.value)
const safePlanStateTag = computed(() => planStateInfo(t.value?.change.planState ?? 'none'))
const safeApprovalStatusTag = computed(() =>
  approvalStatusType(t.value?.approval.status ?? 'pending'),
)
const safeExecutionVerdictTag = computed(() =>
  executionVerdictType(t.value?.execution.verdict ?? 'unrecorded'),
)
const safeRollbackStrategyTag = computed(() =>
  rollbackStrategyType(t.value?.rollback.strategy ?? 'config-revert'),
)

// 这三个标志直接读 wire 上的布尔位，而不是从数组长度推断：后端说
// 「有 SQL / 有回滚动作」是一个事实，推断出来的长度是另一个。
const hasSql = computed(() => t.value?.sql.hasSql ?? false)
const hasFindings = computed(() => (t.value?.sql.review?.findings?.length ?? 0) > 0)
const hasUndo = computed(() => t.value?.rollback.hasUndo ?? false)
const hasApproval = computed(() => t.value?.approval.hasApproval ?? false)
const hasDecisions = computed(() => (t.value?.approval.decisions?.length ?? 0) > 0)
const auditState = computed(() => {
  const a = t.value?.audit
  if (!a) return { label: '未知', type: 'info' as const }
  if (a.verifyError) return { label: '校验出错', type: 'danger' as const }
  if (!a.verified && a.unverifiable) return { label: '未验证', type: 'warning' as const }
  if (a.valid) return { label: '校验通过', type: 'success' as const }
  return { label: '未验证', type: 'warning' as const }
})

function ts(value: string | number | undefined | null): string {
  if (value === undefined || value === null || value === '') return '—'
  return formatTimestamp(value)
}
</script>

<template>
  <div class="ticket-view">
    <PageHeader title="数据库变更发布单" :subtitle="changeId">
      <template #actions>
        <el-button :icon="'Back'" @click="router.back()">返回</el-button>
        <el-button :icon="'Refresh'" :loading="loading" @click="load">刷新</el-button>
      </template>
    </PageHeader>

    <div v-loading="loading" class="ticket-view__body">
      <StateBlock
        v-if="!t && loadTier !== 'ok'"
        :tier="loadTier"
        :message="loadText"
        dense
      />

      <template v-if="t">
        <!-- 概览条 -->
        <ElCard shadow="never" class="ticket-head">
          <div class="ticket-head__row">
            <div class="ticket-head__id">
              <span class="ticket-head__label">{{ t.change.label || t.change.id }}</span>
              <span class="ticket-head__sub">{{ t.change.id }}</span>
            </div>
            <ElSpace>
              <ElTag :type="safePlanStateTag.type" effect="light">{{ safePlanStateTag.label }}</ElTag>
              <ElTag v-if="hasApproval" :type="safeApprovalStatusTag" effect="light">
                {{ approvalStatusLabel(t.approval.status) }}
              </ElTag>
              <ElTag :type="safeExecutionVerdictTag" effect="light">
                {{ executionVerdictLabel(t.execution.verdict) }}
              </ElTag>
              <ElTag :type="safeRollbackStrategyTag" effect="light">
                {{ rollbackStrategyLabel(t.rollback.strategy) }}
              </ElTag>
            </ElSpace>
          </div>
        </ElCard>

        <!-- 计划不可读：说「未知」，不说「没有」 -->
        <ElAlert
          v-if="t.change.planState === 'unreadable'"
          type="error"
          :closable="false"
          show-icon
          class="ticket-alert"
          title="计划制品不可读：SQL、审核结论与回滚动作均为未知"
          :description="`该变更已生成计划（plan_hash: ${t.change.planHash || '—'}），但解析失败。以上三段无法从计划中提取，不能读作「没有」。`"
        />

        <ElTabs type="border-card">
          <!-- 1. 变更信息 -->
          <ElTabPane label="变更信息" name="change">
            <ElDescriptions border :column="3" size="small">
              <ElDescriptionsItem label="变更 ID" :span="2">{{ t.change.id }}</ElDescriptionsItem>
              <ElDescriptionsItem label="环境">{{ t.change.environment || '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="优先级">{{ t.change.priority ? priorityLabel(t.change.priority) : '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="状态">
                <StatusTag :status="t.change.status" />
              </ElDescriptionsItem>
              <ElDescriptionsItem label="创建者">{{ t.change.creator || '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="工作流">{{ t.change.workflow || '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="模板">{{ t.change.template || '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="计划哈希" class="monospace">{{ t.change.planHash || '—' }}</ElDescriptionsItem>
              <ElDescriptionsItem label="创建时间">{{ ts(t.change.createdAt) }}</ElDescriptionsItem>
              <ElDescriptionsItem label="更新时间">{{ ts(t.change.updatedAt) }}</ElDescriptionsItem>
            </ElDescriptions>
          </ElTabPane>

          <!-- 2. SQL 与审核 -->
          <ElTabPane label="SQL 与审核" name="sql">
            <ElCard shadow="never">
              <template #header>SQL 步骤</template>
              <ElTable v-if="hasSql" :data="t.sql.steps" border size="small" stripe>
                <ElTableColumn prop="step" label="步骤" width="180" />
                <ElTableColumn prop="sql" label="SQL" min-width="320" />
              </ElTable>
              <ElEmpty
                v-else
                :description="t.change.planState === 'unreadable'
                  ? '计划不可读，SQL 未知（不是「没有 SQL」）'
                  : '该计划没有可审核的 SQL 步骤'"
              />
            </ElCard>

            <ElCard shadow="never" class="section-gap">
              <template #header>审核结论</template>
              <ElDescriptions v-if="t.sql.review" border :column="3" size="small">
                <ElDescriptionsItem label="结论">
                  <ElTag
                    :type="t.sql.review.verdict === 'pass' ? 'success' : t.sql.review.verdict === 'warn' ? 'warning' : 'danger'"
                    effect="light"
                  >
                    {{ t.sql.review.verdict }}
                  </ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="fail 数">{{ t.sql.review.failCount }}</ElDescriptionsItem>
                <ElDescriptionsItem label="warn 数">{{ t.sql.review.warnCount }}</ElDescriptionsItem>
              </ElDescriptions>
              <ElEmpty v-else description="无审核结论（该计划没有可审核的 SQL 面）" />

              <ElTable v-if="hasFindings" :data="t.sql.review?.findings ?? []" border size="small" stripe class="section-gap">
                <ElTableColumn prop="rule" label="规则" width="120" />
                <ElTableColumn prop="severity" label="级别" width="100" />
                <ElTableColumn prop="step" label="步骤" width="160" />
                <ElTableColumn prop="statement" label="语句号" width="90" />
                <ElTableColumn prop="message" label="说明" min-width="280" />
              </ElTable>
            </ElCard>
          </ElTabPane>

          <!-- 3. 回滚方案 -->
          <ElTabPane label="回滚方案" name="rollback">
            <ElCard shadow="never">
              <ElDescriptions border :column="3" size="small">
                <ElDescriptionsItem label="策略">{{ rollbackStrategyLabel(t.rollback.strategy) }}</ElDescriptionsItem>
                <ElDescriptionsItem label="失败时">{{ t.rollback.onFailure || '—' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="回滚后校验">
                  {{ t.rollback.verifyAfter ? '是' : '否' }}
                </ElDescriptionsItem>
                <ElDescriptionsItem label="快照路径" :span="3">
                  <span v-if="(t.rollback.snapshotPaths?.length ?? 0) === 0">—</span>
                  <span v-for="p in t.rollback.snapshotPaths ?? []" :key="p" class="mono-block">{{ p }}</span>
                </ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>

            <ElCard v-if="hasUndo" shadow="never" class="section-gap">
              <template #header>步骤级回滚动作</template>
              <ElTable :data="t.rollback.stepUndos" border size="small" stripe>
                <ElTableColumn prop="step" label="步骤" width="180" />
                <ElTableColumn prop="strategy" label="策略" width="140" />
                <ElTableColumn prop="undoSteps" label="回退步数" width="110" />
                <ElTableColumn label="不可逆" width="100">
                  <template #default="scope">
                    <ElTag :type="scope.row.irreversible ? 'danger' : 'info'" effect="light">
                      {{ scope.row.irreversible ? '是' : '否' }}
                    </ElTag>
                  </template>
                </ElTableColumn>
              </ElTable>
            </ElCard>
            <ElEmpty
              v-else
              class="section-gap"
              :description="t.change.planState === 'unreadable'
                ? '计划不可读，回滚动作未知（不是「没有回滚」）'
                : '未声明步骤级回滚动作'"
            />
          </ElTabPane>

          <!-- 4. 变更窗口 -->
          <ElTabPane label="变更窗口" name="window">
            <ElCard shadow="never">
              <ElDescriptions border :column="2" size="small">
                <ElDescriptionsItem label="执行点">{{ t.window.enforcedAt || '—' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="随 run 持久化">
                  {{ t.window.persistedPerRun ? '是' : '否' }}
                </ElDescriptionsItem>
                <ElDescriptionsItem label="说明" :span="2">{{ t.window.note || '—' }}</ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>
          </ElTabPane>

          <!-- 5. 审批链 -->
          <ElTabPane label="审批链" name="approval">
            <ElCard shadow="never">
              <template #header>审批概况</template>
              <ElDescriptions border :column="3" size="small">
                <ElDescriptionsItem label="档位">{{ t.approval.level || '—' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="状态">
                  <ElTag v-if="hasApproval" :type="safeApprovalStatusTag" effect="light">
                    {{ approvalStatusLabel(t.approval.status) }}
                  </ElTag>
                  <span v-else>—</span>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="审批人">
                  <span v-if="(t.approval.approvers?.length ?? 0) === 0">—</span>
                  <span v-for="a in t.approval.approvers ?? []" :key="a" class="mono-block">{{ a }}</span>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="待批">{{ t.approval.pendingCount }}</ElDescriptionsItem>
                <ElDescriptionsItem label="已通过">{{ t.approval.approvedCount }}</ElDescriptionsItem>
                <ElDescriptionsItem label="已拒绝">{{ t.approval.rejectedCount }}</ElDescriptionsItem>
                <ElDescriptionsItem label="绑定当前计划" :span="3">
                  <ElTag :type="t.approval.matchesPlan ? 'success' : 'warning'" effect="light">
                    {{ t.approval.matchesPlan ? '是' : '否' }}
                  </ElTag>
                  <ElTag v-if="t.approval.legacyApproval" type="info" effect="light" class="tag-gap">legacy（未绑定任何计划）</ElTag>
                  <ElTag v-if="t.approval.staleApproval" type="danger" effect="light" class="tag-gap">stale（绑定的是旧计划）</ElTag>
                </ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>

            <ElCard v-if="hasDecisions" shadow="never" class="section-gap">
              <template #header>审批决定</template>
              <ElTable :data="t.approval.decisions" border size="small" stripe>
                <ElTableColumn prop="approver" label="审批人" width="160" />
                <ElTableColumn prop="action" label="动作" width="100">
                  <template #default="scope">
                    <ElTag :type="scope.row.action === 'approve' ? 'success' : 'danger'" effect="light">
                      {{ scope.row.action }}
                    </ElTag>
                  </template>
                </ElTableColumn>
                <ElTableColumn prop="reason" label="理由" min-width="220" />
                <ElTableColumn label="决定时间" width="200">
                  <template #default="scope">{{ ts(scope.row.decidedAt) }}</template>
                </ElTableColumn>
              </ElTable>
            </ElCard>
            <ElEmpty v-else class="section-gap" description="该变更没有审批记录" />
          </ElTabPane>

          <!-- 6. 执行证据 -->
          <ElTabPane label="执行证据" name="execution">
            <ElCard shadow="never">
              <ElDescriptions border :column="3" size="small">
                <ElDescriptionsItem label="裁决">
                  <ElTag :type="safeExecutionVerdictTag" effect="light">
                    {{ executionVerdictLabel(t.execution.verdict) }}
                  </ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="有证据">{{ t.execution.hasEvidence ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="批次数">{{ t.execution.batches }}</ElDescriptionsItem>
                <ElDescriptionsItem label="步骤数">{{ t.execution.steps }}</ElDescriptionsItem>
                <ElDescriptionsItem label="目标总数">{{ t.execution.totalTargets }}</ElDescriptionsItem>
                <ElDescriptionsItem label="说明" :span="3">{{ t.execution.reason || '—' }}</ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>
          </ElTabPane>

          <!-- 7. 审计验证 -->
          <ElTabPane label="审计验证" name="audit">
            <ElCard shadow="never">
              <ElDescriptions border :column="3" size="small">
                <ElDescriptionsItem label="结论">
                  <ElTag :type="auditState.type" effect="light">{{ auditState.label }}</ElTag>
                </ElDescriptionsItem>
                <ElDescriptionsItem label="链有效">{{ t.audit.valid ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="trace 条数">{{ t.audit.count }}</ElDescriptionsItem>
                <ElDescriptionsItem label="legacy 条数">{{ t.audit.legacyCount }}</ElDescriptionsItem>
                <ElDescriptionsItem label="已尝试校验">{{ t.audit.verified ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="不可验证">{{ t.audit.unverifiable ? '是' : '否' }}</ElDescriptionsItem>
                <ElDescriptionsItem label="校验错误" :span="3">{{ t.audit.verifyError || '—' }}</ElDescriptionsItem>
              </ElDescriptions>
            </ElCard>

            <ElAlert
              v-if="t.audit.verifyError"
              type="error"
              :closable="false"
              show-icon
              class="section-gap"
              title="审计链校验出错"
              :description="t.audit.verifyError"
            />
          </ElTabPane>
        </ElTabs>
      </template>
    </div>
  </div>
</template>

<style scoped>
.ticket-view__body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.ticket-head__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}

.ticket-head__id {
  display: flex;
  flex-direction: column;
}

.ticket-head__label {
  font-weight: 600;
}

.ticket-head__sub {
  font-size: 12px;
  opacity: 0.7;
  font-family: 'JetBrains Mono', monospace;
}

.ticket-alert {
  margin: 0;
}

.section-gap {
  margin-top: 12px;
}

.mono-block {
  display: block;
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
}

.tag-gap {
  margin-left: 6px;
}
</style>
