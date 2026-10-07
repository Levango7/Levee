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
import { formatTimestamp } from '@/utils/format'
import { batchBarStatus, batchProgress, batchTagType } from '@/utils/batch'

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
// `interrupted` (the real "the run stopped mid-batch" state) had no rendering at
// all. Binding to the server DTO is what keeps the two vocabularies from drifting.
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

function levelTag(level: LogEntry['level']): 'info' | 'success' | 'warning' | 'danger' {
  switch (level) {
    case 'DEBUG':
      return 'info'
    case 'INFO':
      return 'success'
    case 'WARN':
      return 'warning'
    case 'ERROR':
      return 'danger'
    default:
      return 'info'
  }
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
</script>

<template>
  <div class="levee-page">
    <h2 class="levee-page__title">实时监控<template v-if="change"> — {{ change.label }}</template></h2>

    <el-row :gutter="16">
      <!-- Left: change summary + batches + gates -->
      <el-col :span="10">
        <el-card shadow="never" class="levee-card" v-loading="loading">
          <template #header>变更概览</template>
          <template v-if="change">
            <el-descriptions :column="2" border>
              <el-descriptions-item label="ID">{{ change.id }}</el-descriptions-item>
              <el-descriptions-item label="状态"><StatusTag :status="change.status" /></el-descriptions-item>
              <el-descriptions-item label="优先级">{{ change.priority }}</el-descriptions-item>
              <el-descriptions-item label="环境">{{ change.environment }}</el-descriptions-item>
              <el-descriptions-item label="创建时间">{{ formatTimestamp(change.createdAt) }}</el-descriptions-item>
              <el-descriptions-item label="更新时间">{{ formatTimestamp(change.updatedAt) }}</el-descriptions-item>
            </el-descriptions>
          </template>
          <el-empty v-else description="未选择变更" />
        </el-card>

        <el-card shadow="never" class="levee-card">
          <template #header>批次进度</template>
          <el-empty v-if="!summary || summary.batches.length === 0" description="暂无批次记录（该变更还没有进入分批执行）" />
          <template v-else>
            <div class="batch-summary">
              共 {{ summary.total_batches }} 批 · 已完成 {{ summary.done_batches }} 批<template v-if="summary.current_batch_no > 0">
                · 当前第 {{ summary.current_batch_no }} 批</template>
            </div>
            <div v-for="b in summary.batches" :key="b.batch_no" class="batch-item">
              <div class="batch-item__head">
                <span>批次 #{{ b.batch_no }}</span>
                <el-tag size="small" :type="batchTagType(b.status)">{{ b.status }}</el-tag>
              </div>
              <el-progress :percentage="batchProgress(b)" :status="batchBarStatus(b.status)" />
              <div class="batch-item__hosts">
                {{ b.total_hosts }} 台 · 成功 {{ b.succeeded }} · 失败 {{ b.failed }}
              </div>
            </div>
          </template>
        </el-card>

        <el-card shadow="never" class="levee-card">
          <template #header>门禁状态</template>
          <el-empty description="暂无法显示：门禁结论按 run 记在引擎里，但没有对外读路由（POST /gates/verify 只做即时求值，不返回历史）。这里不放占位数据——上一版写的三行 pending 是编造的。" />
        </el-card>
      </el-col>

      <!-- Right: live log stream -->
      <el-col :span="14">
        <el-card shadow="never" class="levee-card log-card">
          <template #header>
            <div class="log-header">
              <span>实时日志</span>
              <div>
                <el-switch v-model="autoScroll" inline-prompt active-text="自动滚动" />
                <el-button text @click="loadLogs">刷新</el-button>
              </div>
            </div>
          </template>
          <div ref="logStreamRef" class="log-stream">
            <div v-for="(log, idx) in logs" :key="idx" class="log-line">
              <span class="log-line__ts">{{ log.timestamp }}</span>
              <el-tag :type="levelTag(log.level)" size="small" effect="plain">{{ log.level }}</el-tag>
              <span class="log-line__source">[{{ log.source }}]</span>
              <span class="log-line__msg">{{ log.message }}</span>
            </div>
            <el-empty v-if="logs.length === 0" description="暂无日志" />
          </div>
        </el-card>
      </el-col>
    </el-row>
  </div>
</template>

<style scoped>
.batch-summary {
  font-size: 12px;
  color: #606266;
  margin-bottom: 8px;
}
.batch-item + .batch-item {
  margin-top: 12px;
}
.batch-item__head {
  display: flex;
  justify-content: space-between;
  margin-bottom: 4px;
}
.batch-item__hosts {
  margin-top: 4px;
  font-size: 12px;
  color: #909399;
}

.log-card {
  height: calc(100vh - 200px);
}
.log-header {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.log-stream {
  height: calc(100% - 60px);
  overflow-y: auto;
  font-family: 'Consolas', 'Menlo', monospace;
  font-size: 12px;
  background: #fafafa;
  padding: 8px;
  border-radius: 4px;
}
.log-line {
  display: flex;
  gap: 6px;
  align-items: center;
  padding: 2px 0;
  border-bottom: 1px solid #f0f0f0;
}
.log-line__ts {
  color: #909399;
  flex-shrink: 0;
}
.log-line__source {
  color: #409eff;
  flex-shrink: 0;
}
.log-line__msg {
  white-space: pre-wrap;
  word-break: break-all;
}
</style>
