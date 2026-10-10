<script setup lang="ts">
// Change detail: what the roadmap's P0 calls the lifecycle page — a run's
// fingerprints (plan, approval, gates, evidence) assembled in one place,
// instead of the list row that used to be the only entry point (and
// /changes/:id routed straight back to the list).
//
// Every panel is fed by an endpoint that already existed; what was missing was
// the assembly:
//
//   - changesApi.get            → the change's own fields (status, env, team,
//                                 template, priority, creator, timestamps)
//   - changesApi.trace(verify)  → the lifecycle timeline AND the hash-chain
//                                 verdict in one call (the chain is the
//                                 evidence; showing the timeline without its
//                                 verdict would be showing the story without
//                                 saying whether it is intact)
//   - batchApi.batchStatus      → per-batch progress, rendered with the batch
//                                 vocabulary's own helpers (utils/batch.ts)
//   - changesApi.logs           → a bounded tail, labelled as a tail
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
import { remediateRollback } from '@/composables/useRollbackRemediation'
import { batchBarStatus, batchLabel, batchProgress } from '@/utils/batch'
import {
  formatTimestamp,
  isArchivableStatus,
  isCancellableStatus,
  isPausableStatus,
  isRemediableStatus,
  isResumableStatus,
  isRetryableStatus,
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
const logs = ref<LogEntry[]>([])
const logsTruncated = ref(false)

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

async function load(): Promise<void> {
  if (!changeId.value) return
  loading.value = true
  try {
    const [c, t, b] = await Promise.all([
      changesApi.get(changeId.value),
      changesApi.trace(changeId.value, { verify: true }),
      batchApi.batchStatus(changeId.value).catch(() => null),
    ])
    change.value = c
    chain.value = { valid: t.hashChainValid, entries: t.entries || [], message: t.verificationMessage || '' }
    batches.value = b
  } catch (err) {
    ElMessage.error(`加载变更失败：${(err as { message?: string })?.message}`)
  } finally {
    loading.value = false
  }
}

async function loadLogs(): Promise<void> {
  try {
    const res = await changesApi.logs(changeId.value, { limit: 200 })
    logs.value = res.entries || []
    logsTruncated.value = res.truncated
  } catch (err) {
    ElMessage.error(`加载日志失败：${(err as { message?: string })?.message}`)
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
          <el-descriptions-item label="优先级">{{ change.priority }}</el-descriptions-item>
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
        <el-timeline v-if="timeline.length > 0">
          <el-timeline-item
            v-for="e in timeline"
            :key="e.id"
            :timestamp="formatTimestamp(e.timestamp)"
            placement="top"
          >
            <div class="tl">
              <span class="tl__action lv-mono">{{ e.action }}</span>
              <span v-if="e.targetHost" class="tl__host lv-mono">{{ e.targetHost }}</span>
              <span v-if="e.actor" class="tl__host">{{ e.actor }}</span>
              <span v-if="e.durationMs" class="detail__dim">{{ e.durationMs }} ms</span>
            </div>
            <div v-if="e.detail" class="tl__detail">{{ e.detail }}</div>
          </el-timeline-item>
        </el-timeline>
        <el-empty v-else description="暂无 trace：该变更还没有进入执行" :image-size="60" />
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
        <div v-else class="detail__dim">尚未加载。</div>
      </div>
    </div>

    <el-empty v-else-if="!loading" description="找不到这个变更" :image-size="80" />
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

.tl {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2, 8px);
  flex-wrap: wrap;
}

.tl__action {
  font-weight: 600;
}

.tl__host {
  color: var(--el-text-color-secondary);
  font-size: 12px;
}

.tl__detail {
  color: var(--el-text-color-secondary);
  font-size: 12px;
  margin-top: 2px;
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
</style>
