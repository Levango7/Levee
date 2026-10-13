<script setup lang="ts">
// Change detail: what the roadmap's P0 calls the lifecycle page — one place to
// read a run's own fields, its approval chain, its audit trail and its batch
// progress, instead of the list row that used to be the only entry point (and
// /changes/:id routed straight back to the list).
//
// Every panel is fed by an endpoint that already existed; what was missing was
// the assembly:
//
//   - changesApi.get            → the change's own fields (status, env, team,
//                                 template, priority, creator, timestamps)
//   - changesApi.approvals      → the approval chain(s), read-only: who must
//                                 decide, how many votes the quorum needs, who
//                                 has decided and on which plan version.
//                                 Approving/rejecting stays in the approval
//                                 centre (ApprovalView); this panel only answers
//                                 "where is the sign-off", and derives its
//                                 counts through utils/approvalChain rather than
//                                 recomputing them here.
//   - changesApi.trace(verify)  → the lifecycle timeline AND the hash-chain
//                                 verdict in one call (the chain is the
//                                 evidence; showing the timeline without its
//                                 verdict would be showing the story without
//                                 saying whether it is intact)
//   - batchApi.batchStatus      → per-batch progress, rendered with the batch
//                                 vocabulary's own helpers (utils/batch.ts)
//   - changesApi.logs           → a bounded tail, labelled as a tail
//
// Two fingerprints this page used to claim but cannot show yet, recorded here so
// the next reader does not re-discover them as a surprise: the generated PLAN
// (GetChange's `include_plan` is accepted at the REST edge but dropped in the
// service, because pb.Change has no field for it and no path returns the stored
// run.plan_json — it needs a proto change) and the per-GATE verdicts (the engine
// holds verify.GateResult in memory and emits it as an event/metric, but nothing
// persists it, so there is no history to read).
//
// Actions are gated by the same helpers the server's switches mirror
// (utils/format.ts): a button appears exactly where the RPC would accept it.
// Remediation is the shared flow (composables/useRollbackRemediation), so this
// page and the list cannot drift on the dry-run-then-commit order.
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'

import { batchApi, changesApi, type BatchSummaryDTO } from '@/api'
import type { Change, LogEntry, TraceEntry } from '@/types/levee'
import PageHeader from '@/components/PageHeader.vue'
import StatusTag from '@/components/StatusTag.vue'
import StateBlock from '@/components/StateBlock.vue'
import TraceTimeline from '@/components/TraceTimeline.vue'
import { remediateRollback } from '@/composables/useRollbackRemediation'
import { approvalSummary } from '@/utils/approvalChain'
import { batchBarStatus, batchLabel, batchProgress } from '@/utils/batch'
import { classifyError, errorMessage } from '@/utils/apiState'
import type { LoadTier } from '@/utils/apiState'
import {
  formatTimestamp,
  isArchivableStatus,
  isCancellableStatus,
  isPausableStatus,
  isRemediableStatus,
  isResumableStatus,
  isRetryableStatus,
  priorityLabel,
} from '@/utils/format'

const route = useRoute()
const router = useRouter()

const changeId = computed(() => String(route.params.id || ''))
const loading = ref(false)
const change = ref<Change | null>(null)
const chain = ref<{ valid: boolean; entries: TraceEntry[]; message: string }>({
  valid: false,
  entries: [],
  message: '',
})
const batches = ref<BatchSummaryDTO | null>(null)
const approvals = ref<Awaited<ReturnType<typeof changesApi.approvals>>>([])
const logs = ref<LogEntry[]>([])
const logsTruncated = ref(false)

// Region states (utils/apiState.ts). The page load and the log tail are two
// fetches with two answers: one failure must not take the other's panel down
// with it, and neither of them may masquerade as an empty change.
const loadError = ref<unknown>(null)
const logsError = ref<unknown>(null)
const logsLoading = ref(false)

const loadTier = computed<LoadTier>(() =>
  loadError.value === null ? 'ok' : classifyError(loadError.value),
)
const loadText = computed(() => errorMessage(loadError.value, '加载变更失败'))

const logsTier = computed<LoadTier>(() => {
  if (logsError.value !== null) return classifyError(logsError.value)
  if (!logsLoading.value && logs.value.length === 0) return 'empty'
  return 'ok'
})
const logsText = computed(() => errorMessage(logsError.value, '加载日志失败'))

// The timeline reads newest-first: an operator arriving at a failed change
// wants the verdict and the last thing that happened, not the beginning.
const timeline = computed(() => [...chain.value.entries].sort((a, b) => b.timestamp - a.timestamp))

const batchRows = computed(() =>
  (batches.value?.batches || []).map((b) => ({
    ...b,
    label: batchLabel(b.status),
    bar: batchBarStatus(b.status),
    // batchProgress returns the percentage itself (clamped); the "done" count
    // is the accounted hosts, which is what the clamp is about.
    progress: batchProgress(b),
    done: b.succeeded + b.failed,
  })),
)

// The approval chains, with the two derived facts (decided / pending) added by
// the shared helper. Kept read-only here: the actionable card lives in
// ApprovalView, and both go through approvalSummary so neither can drift on how
// a vote is counted or who is still owed one.
const approvalRows = computed(() => approvalSummary(approvals.value))

async function load(): Promise<void> {
  if (!changeId.value) return
  loading.value = true
  loadError.value = null
  try {
    const [c, t, b, a] = await Promise.all([
      changesApi.get(changeId.value),
      changesApi.trace(changeId.value, { verify: true }),
      batchApi.batchStatus(changeId.value).catch(() => null),
      changesApi.approvals(changeId.value).catch(() => []),
    ])
    change.value = c
    chain.value = { valid: t.hashChainValid, entries: t.entries || [], message: t.verificationMessage || '' }
    batches.value = b
    approvals.value = a
  } catch (err) {
    // A missing change stays the el-empty below (a real "nothing here"); a
    // forbidden or broken fetch now answers in place instead of leaving a
    // blank page under a toast that names no field.
    loadError.value = err
  } finally {
    loading.value = false
  }
}

async function loadLogs(): Promise<void> {
  logsLoading.value = true
  logsError.value = null
  try {
    const res = await changesApi.logs(changeId.value, { limit: 200 })
    logs.value = res.entries || []
    logsTruncated.value = res.truncated
  } catch (err) {
    logsError.value = err
  } finally {
    logsLoading.value = false
  }
}

// --- actions: each one is offered only where the server would accept it -----

async function retry(): Promise<void> {
  if (!change.value) return
  try {
    await ElMessageBox.confirm(
      `确认重试变更「${change.value.label}」？将按已存储计划重新执行。`,
      '重试变更',
      { confirmButtonText: '重试', cancelButtonText: '取消', type: 'warning' },
    )
  } catch {
    return
  }
  try {
    await changesApi.retry(change.value.id, {})
    ElMessage.success('重试已提交')
    load()
  } catch (err) {
    ElMessage.error(`重试失败：${(err as { message?: string })?.message}`)
  }
}

async function pause(): Promise<void> {
  await transition('暂停', '暂停变更', () => changesApi.pause(changeId.value, 'paused from detail page'))
}

async function resume(): Promise<void> {
  await transition('恢复', '恢复变更', () => changesApi.resume(changeId.value, 'resumed from detail page'))
}

async function cancel(): Promise<void> {
  await transition('取消', '取消变更', () => changesApi.cancel(changeId.value, { force: false }))
}

async function archive(): Promise<void> {
  await transition(
    '归档',
    '归档变更',
    () => changesApi.archive(changeId.value, false),
    '归档后该变更移出活动列表；用它自己的历史做结论（不删除产物）。',
  )
}

async function transition(
  verb: string,
  title: string,
  call: () => Promise<unknown>,
  note = '',
): Promise<void> {
  if (!change.value) return
  try {
    await ElMessageBox.confirm(`${verb}变更「${change.value.label}」？${note}`, title, {
      confirmButtonText: verb,
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await call()
    ElMessage.success(`${verb}已提交`)
    load()
  } catch (err) {
    ElMessage.error(`${verb}失败：${(err as { message?: string })?.message}`)
  }
}

async function remediate(): Promise<void> {
  if (!change.value) return
  if (await remediateRollback(change.value)) load()
}

onMounted(load)
</script>

<template>
  <div class="levee-page">
    <PageHeader :title="change ? change.label : '变更详情'" :description="changeId">
      <template #actions>
        <el-button :icon="'Back'" @click="router.push('/changes')">返回看板</el-button>
        <el-button :icon="'Refresh'" :loading="loading" @click="load">刷新</el-button>
        <el-button type="primary" @click="router.push(`/monitor/${changeId}`)">实时监控</el-button>
      </template>
    </PageHeader>

    <div v-if="change" class="detail">
      <!-- Fingerprints -->
      <div class="lv-panel">
        <div class="lv-panel__head"><h3 class="detail__h">指纹</h3></div>
        <el-descriptions :column="3" border size="small">
          <el-descriptions-item label="状态"><StatusTag :status="change.status" /></el-descriptions-item>
          <el-descriptions-item label="环境">{{ change.environment || '（未声明）' }}</el-descriptions-item>
          <el-descriptions-item label="团队">{{ change.team || '—' }}</el-descriptions-item>
          <el-descriptions-item label="模板">{{ change.templateName || '—' }}</el-descriptions-item>
          <el-descriptions-item label="优先级">{{ priorityLabel(change.priority) }}</el-descriptions-item>
          <el-descriptions-item label="创建人">{{ change.createdBy || change.requester || '—' }}</el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ formatTimestamp(change.createdAt) }}</el-descriptions-item>
          <el-descriptions-item label="更新时间">{{ formatTimestamp(change.updatedAt) }}</el-descriptions-item>
          <el-descriptions-item label="参数">
            <span v-if="Object.keys(change.params || {}).length === 0">—</span>
            <span v-else class="lv-mono">
              {{ Object.entries(change.params).map(([k, v]) => `${k}=${v}`).join('  ') }}
            </span>
          </el-descriptions-item>
        </el-descriptions>
      </div>

      <!-- Actions, gated like the server -->
      <div class="lv-panel">
        <div class="lv-panel__head"><h3 class="detail__h">操作</h3></div>
        <div class="detail__actions">
          <el-button v-if="isRemediableStatus(change.status)" type="danger" @click="remediate">
            补救未完成的回滚
          </el-button>
          <el-button v-if="isRetryableStatus(change.status)" type="warning" @click="retry">重试</el-button>
          <el-button v-if="isPausableStatus(change.status)" @click="pause">暂停</el-button>
          <el-button v-if="isResumableStatus(change.status)" @click="resume">恢复</el-button>
          <el-button v-if="isCancellableStatus(change.status)" @click="cancel">取消</el-button>
          <el-button v-if="isArchivableStatus(change.status)" @click="archive">归档</el-button>
          <span v-if="
            !isRemediableStatus(change.status) && !isRetryableStatus(change.status) &&
            !isPausableStatus(change.status) && !isResumableStatus(change.status) &&
            !isCancellableStatus(change.status) && !isArchivableStatus(change.status)
          " class="detail__dim">当前状态没有可执行的动作</span>
        </div>
      </div>

      <!-- Evidence chain + timeline -->
      <div class="lv-panel">
        <div class="lv-panel__head">
          <h3 class="detail__h">生命周期与证据链</h3>
          <el-tag :type="chain.valid ? 'success' : 'danger'" size="small">
            {{ chain.valid ? '哈希链完整' : '哈希链未通过' }}
          </el-tag>
        </div>
        <p class="detail__dim">
          共 {{ chain.entries.length }} 条 trace{{ chain.message ? ` · ${chain.message}` : '' }}
        </p>
        <TraceTimeline :entries="timeline" empty-text="暂无 trace：该变更还没有进入执行" />
      </div>

      <!-- Approval chain: a read-only digest of where the sign-off stands. The
           actionable card (approve / reject / delegate) lives in the approval
           centre; here we only surface the quorum facts and who has already
           decided, so an operator reading this page can see whether the change
           is cleared to run without leaving it. Counts come from
           utils/approvalChain, the same helper the card uses, so the two cannot
           disagree on how a vote is counted or who is still owed one. -->
      <div class="lv-panel">
        <div class="lv-panel__head">
          <h3 class="detail__h">审批链</h3>
          <el-tag size="small" type="info">只读</el-tag>
        </div>
        <p class="detail__dim">批准 / 驳回请在「审批中心」进行，这里只回答"签核到哪一步了"。</p>
        <div v-if="approvalRows.length === 0" class="detail__dim">暂无审批链（该变更尚未进入审批）。</div>
        <div v-else class="chains">
          <div v-for="c in approvalRows" :key="c.id" class="chain">
            <div class="chain__head">
              <span class="chain__level lv-mono">{{ c.level }}</span>
              <span class="detail__dim">需 {{ c.minApprovers }} 票 · 已投 {{ c.decided }} 票</span>
              <span v-if="c.pending.length" class="detail__dim">未投：{{ c.pending.join('、') }}</span>
            </div>
            <p v-if="c.excludeInitiator" class="detail__dim">发起人 {{ c.initiator }} 被排除，不可自批</p>
            <p v-if="c.legacy" class="detail__dim">未绑定计划版本（旧链，任意 plan 均可结算）</p>
            <p v-if="c.stale" class="chain__stale">该批准所依据的计划已非当前版本，需重新批准</p>
            <ul v-if="c.decided" class="chain__decisions">
              <li v-for="d in c.decisions" :key="d.approver">
                <span class="lv-mono">{{ d.approver }}</span>
                <span :class="d.action === 'approve' ? 'chain__ok' : 'chain__no'">
                  {{ d.action === 'approve' ? '同意' : '驳回' }}
                </span>
                <span v-if="d.reason" class="detail__dim">{{ d.reason }}</span>
                <span v-if="d.decidedAt" class="detail__dim lv-mono">{{ formatTimestamp(d.decidedAt) }}</span>
              </li>
            </ul>
          </div>
        </div>
      </div>

      <!-- Batches -->
      <div class="lv-panel">
        <div class="lv-panel__head">
          <h3 class="detail__h">批次</h3>
          <span v-if="batches" class="detail__dim">
            当前第 {{ batches.current_batch_no }} 批 / 共 {{ batches.total_batches }} 批 ·
            完成 {{ batches.done_batches }}
          </span>
        </div>
        <div v-if="batchRows.length === 0" class="detail__dim">暂无批次数据（未计划或未执行）</div>
        <div v-else class="batches">
          <div v-for="b in batchRows" :key="b.batch_no" class="batch">
            <span class="batch__no lv-mono">#{{ b.batch_no }}</span>
            <el-progress
              :percentage="b.progress"
              :status="b.bar"
              :stroke-width="10"
              class="batch__bar"
            />
            <span class="batch__state">{{ b.label }}</span>
            <span class="detail__dim lv-mono">
              {{ b.done }}/{{ b.total_hosts }}（成功 {{ b.succeeded }} · 失败 {{ b.failed }}）
            </span>
          </div>
        </div>
      </div>

      <!-- Log tail -->
      <div class="lv-panel">
        <div class="lv-panel__head">
          <h3 class="detail__h">执行日志（尾部）</h3>
          <el-button size="small" @click="loadLogs">加载尾部 200 行</el-button>
        </div>
        <p class="detail__dim">
          这里是尾部快照，不是实时流；实时跟随请用「实时监控」。
          <span v-if="logsTruncated">（已截断）</span>
        </p>
        <pre v-if="logs.length > 0" class="detail__logs">{{ logs.map((l) => `${l.timestamp} [${l.level}] ${l.message}`).join('\n') }}</pre>
        <StateBlock
          v-else-if="logsTier === 'error' || logsTier === 'forbidden'"
          :tier="logsTier"
          :message="logsText"
          :hint="logsTier === 'forbidden' ? '执行日志受角色管控，请联系管理员授权' : ''"
          dense
          @retry="loadLogs"
        />
        <div v-else class="detail__dim">尚未加载。</div>
      </div>
    </div>

    <el-empty v-else-if="!loading && loadTier === 'ok'" description="找不到这个变更" :image-size="80" />
    <StateBlock
      v-else-if="loadTier === 'error' || loadTier === 'forbidden'"
      :tier="loadTier"
      :message="loadText"
      :hint="loadTier === 'forbidden' ? '该账号的角色看不到这个变更，请联系管理员授权' : ''"
      @retry="load"
    />
  </div>
</template>

<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-4, 16px);
}

.detail__h {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}

.detail__dim {
  color: var(--el-text-color-secondary);
  margin: 0 0 var(--lv-space-2, 8px);
}

.detail__actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--lv-space-2, 8px);
}

.detail__logs {
  margin: 0;
  max-height: 320px;
  overflow: auto;
  padding: var(--lv-space-2, 8px);
  background: var(--el-fill-color-light);
  border-radius: 6px;
  font-family: var(--lv-font-mono, monospace);
  font-size: 12px;
  white-space: pre-wrap;
}

.batches {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-2, 8px);
}

.batch {
  display: grid;
  grid-template-columns: 48px 1fr 88px 200px;
  align-items: center;
  gap: var(--lv-space-2, 8px);
}

.batch__bar {
  min-width: 120px;
}

.batch__state {
  font-size: 13px;
}

.chains {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-3, 12px);
}

.chain__head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--lv-space-2, 8px);
}

.chain__level {
  font-size: 13px;
  font-weight: 600;
}

.chain__stale {
  color: var(--el-color-danger);
  margin: var(--lv-space-1, 4px) 0 0;
  font-size: 12px;
}

.chain__decisions {
  list-style: none;
  margin: var(--lv-space-2, 8px) 0 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.chain__decisions li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--lv-space-2, 8px);
}

.chain__ok {
  color: var(--el-color-success);
}

.chain__no {
  color: var(--el-color-danger);
}
</style>
