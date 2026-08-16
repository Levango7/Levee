<script setup lang="ts">
// MonitorView is the real-time execution monitor. It shows a live log stream
// (polled at a fixed interval since SSE support depends on the gateway), batch
// progress bars and gate (verification) status cards.
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { changesApi } from '@/api'
import type { Change, LogEntry } from '@/types/levee'
import StatusTag from '@/components/StatusTag.vue'
import { formatTimestamp } from '@/utils/format'

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

// Batch progress mock structure. The real /changes/:id/plan endpoint would
// populate this; we keep a local copy so the UI degrades gracefully when the
// plan is unavailable.
interface BatchProgress {
  index: number
  hosts: string[]
  status: 'pending' | 'running' | 'success' | 'failed'
  progress: number
}

const batches = ref<BatchProgress[]>([])

// Gate status cards. Each gate is a verification step the engine runs between
// batches; the UI shows pass/fail/pending.
interface GateStatus {
  name: string
  status: 'pass' | 'fail' | 'pending' | 'running'
  message: string
}

const gates = ref<GateStatus[]>([
  { name: 'pre_check', status: 'pending', message: '前置检查' },
  { name: 'post_check', status: 'pending', message: '后置验证' },
  { name: 'rollback_ready', status: 'pending', message: '回滚就绪' },
])

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

async function refreshAll(): Promise<void> {
  loading.value = true
  await Promise.all([loadChange(), loadLogs()])
  loading.value = false
}

function startPolling(): void {
  stopPolling()
  pollTimer = setInterval(loadLogs, POLL_INTERVAL_MS)
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

function gateTagType(status: GateStatus['status']): 'success' | 'danger' | 'info' | 'primary' {
  switch (status) {
    case 'pass':
      return 'success'
    case 'fail':
      return 'danger'
    case 'running':
      return 'primary'
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
          <el-empty v-if="batches.length === 0" description="暂无批次信息" />
          <div v-for="b in batches" :key="b.index" class="batch-item">
            <div class="batch-item__head">
              <span>批次 #{{ b.index }}</span>
              <el-tag size="small" :type="b.status === 'success' ? 'success' : b.status === 'failed' ? 'danger' : 'primary'">
                {{ b.status }}
              </el-tag>
            </div>
            <el-progress :percentage="b.progress" :status="b.status === 'failed' ? 'exception' : b.status === 'success' ? 'success' : undefined" />
            <div class="batch-item__hosts">{{ b.hosts.join(', ') }}</div>
          </div>
        </el-card>

        <el-card shadow="never" class="levee-card">
          <template #header>门禁状态</template>
          <el-row :gutter="8">
            <el-col v-for="g in gates" :key="g.name" :span="8">
              <div class="gate-card">
                <el-tag :type="gateTagType(g.status)" size="small">{{ g.status }}</el-tag>
                <div class="gate-card__name">{{ g.name }}</div>
                <div class="gate-card__msg">{{ g.message }}</div>
              </div>
            </el-col>
          </el-row>
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

.gate-card {
  text-align: center;
  padding: 8px;
  border: 1px solid var(--levee-border);
  border-radius: 4px;
}
.gate-card__name {
  margin-top: 4px;
  font-size: 13px;
  font-weight: 500;
}
.gate-card__msg {
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
