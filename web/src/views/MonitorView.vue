<script setup lang="ts">
// MonitorView is the real-time execution monitor: a polled log stream (there is
// no SSE route — `WatchChange`/`StreamLogs` exist only as gRPC server streams,
// so the browser polls), batch progress read from the same endpoint the cluster
// page uses, and a gate panel that is deliberately EMPTY until gate verdicts get
// a read route.
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { batchApi, changesApi, type BatchSummaryDTO } from '@/api'
import type { Change, LogEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import PageHeader from '@/components/PageHeader.vue'
import { formatTimestamp } from '@/utils/format'
import { batchBarStatus, batchLabel, batchProgress } from '@/utils/batch'

const route = useRoute()

const changeId = computed(() => (route.params.changeId as string) || '')
const change = ref<Change | null>(null)
const logs = ref<LogEntry[]>([])
const loading = ref(false)
const autoScroll = ref(true)
const logStreamRef = ref<HTMLElement | null>(null)

// Polling interval. 2s is a reasonable trade-off between freshness and load.
const POLL_INTERVAL_MS = 2000
let pollTimer: ReturnType<typeof setInterval> | null = null

// Batch progress is `GET /system/batch-status?run_id=…`. A change id and a run
// id are the same key in this system (ApplyChange and GetRunReport both resolve
// against the run row), so the route param goes straight through.
//
// The status vocabulary is the store's own — pending | running | done | failed |
// interrupted. This file used to declare a LOCAL copy of the shape whose states
// were `pending|running|success|failed`: nothing ever writes `success`, and
// `interrupted` (the real "the run stopped mid-batch" state) had no rendering
// at all. Binding to the server DTO is what keeps the two vocabularies from
// drifting.
const summary = ref<BatchSummaryDTO | null>(null)

// Gate verdicts are recorded per run inside the engine's closure, but there is
// no read route for them: `POST /gates/verify` evaluates on demand and returns
// nothing historical. The card used to render three hardcoded rows all saying
// `pending`, so a run with no gates at all looked like a run whose gates were
// still running. That is removed rather than dressed up — see the panel below.

async function loadChange(): Promise<void> {
  if (!changeId.value) return
  try {
    change.value = await changesApi.get(changeId.value)
  } catch (err) {
    ElMessage.error((err as { message?: string })?.message || '加载变更失败')
  }
}

async function loadLogs(): Promise<void> {
  if (!changeId.value) return
  try {
    const res = await changesApi.logs(changeId.value, { limit: 200 })
    logs.value = res.entries || []
    if (autoScroll.value) {
      await nextTick()
      scrollToBottom()
    }
  } catch {
    // Silent fail during polling to avoid spamming toasts.
  }
}

function scrollToBottom(): void {
  const el = logStreamRef.value
  if (el) {
    el.scrollTop = el.scrollHeight
  }
}

async function loadSummary(): Promise<void> {
  if (!changeId.value) return
  try {
    summary.value = await batchApi.batchStatus(changeId.value)
  } catch {
    // Silent during polling, same policy as the log stream: a transient failure
    // must not raise a toast every 2 seconds. The panel keeps the last real
    // numbers rather than replacing them with anything invented.
  }
}

async function refreshAll(): Promise<void> {
  loading.value = true
  await Promise.all([loadChange(), loadLogs(), loadSummary()])
  loading.value = false
}

function startPolling(): void {
  stopPolling()
  pollTimer = setInterval(() => {
    void loadLogs()
    void loadSummary()
  }, POLL_INTERVAL_MS)
}

function stopPolling(): void {
  if (pollTimer) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

// Log level → tone. Kept as a class name rather than an el-tag type because the
// terminal panel paints its own colours (a tag's chrome would fight the pane).
function levelClass(level: LogEntry['level']): string {
  return `log-line--${(level || 'INFO').toLowerCase()}`
}

// The timestamp column is fixed-width in the pane; logs carry an ISO string and
// only the time-of-day part is useful at a glance, so trim it here rather than
// widening the column for the date everyone already knows.
function logTime(ts: string): string {
  if (!ts) return ''
  const t = ts.includes('T') ? ts.split('T')[1] || ts : ts
  return t.replace('Z', '').slice(0, 12)
}

// When autoScroll is toggled on, immediately jump to the bottom.
watch(autoScroll, (v) => {
  if (v) scrollToBottom()
})

onMounted(async () => {
  await refreshAll()
  startPolling()
})

onUnmounted(stopPolling)

const batchTotals = computed(() => {
  const s = summary.value
  if (!s) return null
  return {
    total: s.total_batches,
    done: s.done_batches,
    current: s.current_batch_no,
  }
})
</script>

<template>
  <div class="levee-page">
    <PageHeader
      :title="change ? `执行监控 · ${change.label}` : '执行监控'"
      description="实时日志与分批推进；进入 run 后每 2 秒轮询一次"
    >
      <template #actions>
        <el-button :icon="'Refresh'" :loading="loading" @click="refreshAll">刷新</el-button>
      </template>
    </PageHeader>

    <div class="monitor">
      <!-- Left column: what is running -->
      <div class="monitor__side">
        <section class="lv-panel" v-loading="loading">
          <div class="lv-panel__head">
            <span class="lv-panel__title">变更概览</span>
            <StatusTag v-if="change" :status="change.status" />
          </div>
          <div class="lv-panel__body">
            <template v-if="change">
              <div class="lv-kv">
                <div class="lv-kv__k">变更 ID</div>
                <div class="lv-kv__v lv-mono">{{ change.id }}</div>
                <div class="lv-kv__k">优先级</div>
                <div class="lv-kv__v">{{ change.priority }}</div>
                <div class="lv-kv__k">环境</div>
                <div class="lv-kv__v">{{ change.environment }}</div>
                <div class="lv-kv__k">模板</div>
                <div class="lv-kv__v lv-mono">{{ change.templateName || '—' }}</div>
                <div class="lv-kv__k">创建时间</div>
                <div class="lv-kv__v lv-mono">{{ formatTimestamp(change.createdAt) }}</div>
                <div class="lv-kv__k">更新时间</div>
                <div class="lv-kv__v lv-mono">{{ formatTimestamp(change.updatedAt) }}</div>
              </div>
            </template>
            <div v-else class="lv-empty">
              <el-icon class="lv-empty__icon"><Aim /></el-icon>
              <span class="lv-empty__title">未指定变更</span>
              <span>从变更看板点进某个变更，或直接用 /monitor/&lt;change-id&gt; 打开。</span>
            </div>
          </div>
        </section>

        <section class="lv-panel">
          <div class="lv-panel__head">
            <span class="lv-panel__title">批次推进</span>
            <span v-if="batchTotals" class="lv-panel__hint lv-mono">
              {{ batchTotals.done }} / {{ batchTotals.total }} 批完成
            </span>
          </div>
          <div class="lv-panel__body">
            <div v-if="!summary || summary.batches.length === 0" class="lv-empty">
              <el-icon class="lv-empty__icon"><Files /></el-icon>
              <span>暂无批次记录</span>
              <span>该变更还没有进入分批执行阶段。</span>
            </div>
            <ol v-else class="batches">
              <li
                v-for="b in summary.batches"
                :key="b.batch_no"
                class="batch"
                :class="{ 'batch--current': b.batch_no === batchTotals?.current }"
              >
                <div class="batch__rail" aria-hidden="true">
                  <span class="batch__node" :class="`batch__node--${b.status}`"></span>
                </div>
                <div class="batch__body">
                  <div class="batch__head">
                    <span class="batch__no lv-mono">批次 #{{ b.batch_no }}</span>
                    <span class="batch__state" :class="`batch__state--${b.status}`">{{ batchLabel(b.status) }}</span>
                  </div>
                  <el-progress
                    :percentage="batchProgress(b)"
                    :status="batchBarStatus(b.status)"
                    :stroke-width="6"
                    :show-text="false"
                  />
                  <div class="batch__meta">
                    <span>{{ b.total_hosts }} 台</span>
                    <span class="batch__ok">成功 {{ b.succeeded }}</span>
                    <span v-if="b.failed" class="batch__bad">失败 {{ b.failed }}</span>
                  </div>
                </div>
              </li>
            </ol>
          </div>
        </section>

        <section class="lv-panel">
          <div class="lv-panel__head">
            <span class="lv-panel__title">门禁状态</span>
          </div>
          <div class="lv-panel__body">
            <div class="lv-empty">
              <el-icon class="lv-empty__icon"><Lock /></el-icon>
              <span class="lv-empty__title">暂无历史门禁结论可读</span>
              <span>
                门禁结论按 run 记录在引擎内部，但没有对外读路由 ——
                <code>POST /gates/verify</code> 只做即时求值，不返回历史。
                这里不放占位数据。
              </span>
            </div>
          </div>
        </section>
      </div>

      <!-- Right column: the live log terminal -->
      <section class="lv-panel terminal-card">
        <div class="lv-panel__head">
          <span class="lv-panel__title">实时日志</span>
          <div class="terminal-tools">
            <span class="terminal-count lv-mono">{{ logs.length }} 行</span>
            <el-switch v-model="autoScroll" inline-prompt active-text="自动滚动" />
            <el-button text :icon="'Refresh'" @click="loadLogs">刷新</el-button>
          </div>
        </div>
        <div ref="logStreamRef" class="terminal">
          <div v-if="logs.length === 0" class="terminal__empty">
            <span class="terminal__prompt lv-mono">$</span>
            <span>等待日志输出…（该 run 尚未产生日志，或日志已被轮转）</span>
          </div>
          <div v-for="(log, idx) in logs" :key="idx" class="log-line" :class="levelClass(log.level)">
            <span class="log-line__ts lv-mono">{{ logTime(log.timestamp) }}</span>
            <span class="log-line__level lv-mono">{{ log.level }}</span>
            <span class="log-line__source lv-mono">{{ log.source }}</span>
            <span class="log-line__msg">{{ log.message }}</span>
          </div>
        </div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.monitor {
  display: grid;
  grid-template-columns: minmax(320px, 400px) minmax(0, 1fr);
  gap: var(--lv-space-4);
  align-items: stretch;
}

@media (max-width: 1180px) {
  .monitor {
    grid-template-columns: minmax(0, 1fr);
  }
}

.monitor__side {
  display: flex;
  flex-direction: column;
  gap: var(--lv-space-4);
}

/* ---------------------------------------------------------------- batches */

.batches {
  margin: 0;
  padding: 0;
  list-style: none;
}

.batch {
  display: grid;
  grid-template-columns: 20px minmax(0, 1fr);
  gap: var(--lv-space-3);
}

.batch__rail {
  position: relative;
  display: flex;
  justify-content: center;
}

/* The connecting rail between nodes: a timeline, so a run's advance reads as a
 * sequence instead of as N unrelated progress bars. */
.batch__rail::before {
  content: '';
  position: absolute;
  top: 0;
  bottom: 0;
  width: 1px;
  background: var(--lv-border);
}

.batch:first-child .batch__rail::before {
  top: 10px;
}

.batch:last-child .batch__rail::before {
  bottom: calc(100% - 10px);
}

.batch__node {
  position: relative;
  z-index: 1;
  width: 9px;
  height: 9px;
  margin-top: 5px;
  border-radius: var(--lv-radius-full);
  background: var(--lv-ink-400);
  box-shadow: 0 0 0 3px var(--lv-surface);
}

html.dark .batch__node {
  background: var(--lv-ink-500);
}

.batch__node--completed,
.batch__node--done {
  background: var(--lv-ok);
}

.batch__node--failed {
  background: var(--lv-bad);
}

.batch__node--rolled_back,
.batch__node--interrupted {
  background: var(--lv-warn);
}

.batch__node--running {
  background: var(--lv-accent);
  animation: lv-pulse 1.8s var(--lv-ease) infinite;
  box-shadow: 0 0 0 3px var(--lv-accent-soft);
}

.batch__body {
  padding-bottom: var(--lv-space-4);
}

.batch:last-child .batch__body {
  padding-bottom: 0;
}

.batch__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--lv-space-2);
  margin-bottom: 6px;
}

.batch__no {
  font-size: var(--lv-text-sm);
  font-weight: 500;
  color: var(--lv-text-1);
}

.batch--current .batch__no {
  color: var(--lv-accent);
}

.batch__state {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.batch__state--completed,
.batch__state--done {
  color: var(--lv-ok);
}

.batch__state--failed {
  color: var(--lv-bad);
}

.batch__state--rolled_back,
.batch__state--interrupted,
.batch__state--running {
  color: var(--lv-warn);
}

.batch__meta {
  display: flex;
  gap: var(--lv-space-3);
  margin-top: 5px;
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

.batch__ok {
  color: var(--lv-ok);
}

.batch__bad {
  color: var(--lv-bad);
}

/* --------------------------------------------------------------- terminal */

.terminal-card {
  display: flex;
  flex-direction: column;
  /* Fills the grid row so the terminal's frame lines up with the left column's
   * bottom edge instead of stopping where its own content ended. */
  height: 100%;
  min-height: 520px;
  overflow: hidden;
}

.terminal-tools {
  display: flex;
  align-items: center;
  gap: var(--lv-space-3);
}

.terminal-count {
  font-size: var(--lv-text-xs);
  color: var(--lv-text-3);
}

/* The log pane is dark in BOTH themes: it is a terminal, and inverting it with
 * the page would both break the operator's expectation and destroy the level
 * colours' contrast. */
.terminal {
  flex: 1;
  min-height: 320px;
  overflow-y: auto;
  padding: var(--lv-space-3) 0;
  background: #0b1016;
  font-family: var(--lv-font-mono);
  font-size: 12px;
  line-height: 1.65;
  border-bottom-left-radius: var(--lv-radius-lg);
  border-bottom-right-radius: var(--lv-radius-lg);
}

.terminal::-webkit-scrollbar-thumb {
  background: rgba(255, 255, 255, 0.16);
  background-clip: content-box;
}

.terminal__empty {
  display: flex;
  align-items: center;
  gap: var(--lv-space-2);
  padding: var(--lv-space-6) var(--lv-space-4);
  color: #6b7c85;
}

.terminal__prompt {
  color: #3fadaa;
}

.log-line {
  display: grid;
  grid-template-columns: 84px 52px 110px minmax(0, 1fr);
  gap: var(--lv-space-2);
  padding: 1px var(--lv-space-4);
  color: #c3ced6;
  border-left: 2px solid transparent;
}

.log-line:hover {
  background: rgba(255, 255, 255, 0.03);
}

.log-line__ts {
  color: #5c6b75;
  font-variant-numeric: tabular-nums;
}

.log-line__level {
  font-weight: 600;
  font-size: 11px;
}

.log-line__source {
  color: #7d8f9a;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.log-line__msg {
  overflow-wrap: anywhere;
}

.log-line--info .log-line__level {
  color: #6fbfa8;
}

.log-line--debug .log-line__level {
  color: #7d8f9a;
}

.log-line--debug .log-line__msg {
  color: #8d9ba4;
}

.log-line--warn {
  border-left-color: rgba(232, 163, 61, 0.55);
}

.log-line--warn .log-line__level {
  color: #e8a33d;
}

.log-line--warn .log-line__msg {
  color: #f0d9b0;
}

.log-line--error {
  border-left-color: rgba(249, 112, 102, 0.6);
  background: rgba(249, 112, 102, 0.06);
}

.log-line--error .log-line__level {
  color: #f97066;
}

.log-line--error .log-line__msg {
  color: #f6cfcb;
}

@media (prefers-reduced-motion: reduce) {
  .batch__node--running {
    animation: none;
  }
}
</style>
