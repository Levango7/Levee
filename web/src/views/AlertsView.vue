<script setup lang="ts">
// AlertsView — the alert centre. One SSE connection (useAlertStream) feeds the
// list; it opens on mount and closes on leave, which is why this page — not
// the shell — owns the connection. The header badge reuses the same module
// state, so the count stays live without a second socket.
//
// The three tiers follow the repo's region contract (utils/apiState.ts +
// regionStates.spec): the stream being unreachable with an empty list is the
// error tier; an empty ring is the empty tier. A stream that dropped AFTER
// delivering the replay is a warning note above real data, not a tier swap.
import { computed, ref } from 'vue'
import { useAlertStream } from '@/composables/useAlertStream'
import { ALERT_SEVERITIES, type AlertSeverity } from '@/types/vocabulary'
import PageHeader from '@/components/PageHeader.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { LoadTier } from '@/utils/apiState'

const { alerts, connected, streamError } = useAlertStream()

const severityFilter = ref<'' | AlertSeverity>('')
const onlyFiring = ref(true)

// Alert status: 0 = firing, 1 = resolved; missing counts as firing (a frame
// with no resolution recorded IS firing — see useAlertStream's badge count).
function isFiring(status?: number | string): boolean {
  return status === undefined || status === 0 || status === '0'
}

function severityOf(a: { severity?: number | string }): AlertSeverity {
  const raw =
    typeof a.severity === 'number'
      ? (ALERT_SEVERITIES[a.severity] ?? 'info')
      : String(a.severity)
  return (ALERT_SEVERITIES as readonly string[]).includes(raw)
    ? (raw as AlertSeverity)
    : 'info'
}

function severityTagType(s: AlertSeverity): 'danger' | 'warning' | 'info' {
  if (s === 'critical') return 'danger'
  if (s === 'warning') return 'warning'
  return 'info'
}

function severityLabel(s: AlertSeverity): string {
  if (s === 'critical') return '严重'
  if (s === 'warning') return '警告'
  return '信息'
}

const visible = computed(() =>
  alerts.value.filter((a) => {
    if (onlyFiring.value && !isFiring(a.status)) return false
    if (severityFilter.value !== '' && severityOf(a) !== severityFilter.value) return false
    return true
  }),
)

const regionTier = computed<LoadTier>(() => {
  // The stream is the page's only data source: unreachable AND empty is the
  // error tier. Connected with an empty ring is the honest empty tier.
  if (streamError.value !== null && alerts.value.length === 0) return 'error'
  if (!connected.value && !streamError.value) return 'empty'
  if (alerts.value.length === 0) return 'empty'
  return 'ok'
})

// starts_at arrives as RFC3339 (json.Marshal of time.Time).
function ago(startsAt?: string): string {
  if (!startsAt) return ''
  const t = new Date(startsAt).getTime()
  if (Number.isNaN(t)) return ''
  const sec = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (sec < 60) return `${sec} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`
  return `${Math.floor(sec / 86400)} 天前`
}

function reloadPage(): void {
  window.location.reload()
}
</script>

<template>
  <div class="levee-page">
    <PageHeader title="告警" hint="实时告警流 — 回放告警环中已知告警，并持续接收新告警" />

    <div class="lv-toolbar">
      <el-select v-model="severityFilter" placeholder="全部级别" clearable style="width: 140px">
        <el-option v-for="s in ALERT_SEVERITIES" :key="s" :label="severityLabel(s)" :value="s" />
      </el-select>
      <el-checkbox v-model="onlyFiring">仅活跃</el-checkbox>
      <span v-if="!connected && streamError" class="stream-note stream-note--warn">
        {{ streamError }}
      </span>
      <span v-else-if="connected" class="stream-note stream-note--ok">● 已连接</span>
    </div>

    <StateBlock
      v-if="regionTier === 'error'"
      tier="error"
      :message="streamError ?? '告警流不可用'"
      @retry="reloadPage"
    />
    <StateBlock
      v-else-if="regionTier === 'empty'"
      tier="empty"
      message="当前没有告警 — 安静是好事"
    />
    <div v-else class="lv-panel table-panel">
      <div class="lv-panel__head">
        <span class="lv-panel__hint">共 {{ visible.length }} 条</span>
      </div>
      <div class="lv-panel__body">
        <el-table :data="visible" stripe>
          <el-table-column label="级别" width="110">
            <template #default="{ row }">
              <el-tag :type="severityTagType(severityOf(row))" size="small">
                {{ severityLabel(severityOf(row)) }}
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="标题" min-width="240" show-overflow-tooltip>
            <template #default="{ row }">
              {{ row.title }}
              <el-tag v-if="!isFiring(row.status)" size="small" type="info" style="margin-left: 6px">
                已恢复
              </el-tag>
            </template>
          </el-table-column>
          <el-table-column label="目标" min-width="160" show-overflow-tooltip>
            <template #default="{ row }">{{ row.target || '—' }}</template>
          </el-table-column>
          <el-table-column prop="source" label="来源" width="140" show-overflow-tooltip />
          <el-table-column label="环境" width="100">
            <template #default="{ row }">{{ row.environment || '—' }}</template>
          </el-table-column>
          <el-table-column label="触发时间" width="130">
            <template #default="{ row }">{{ ago(row.starts_at) }}</template>
          </el-table-column>
        </el-table>
      </div>
    </div>
  </div>
</template>

<style scoped>
.stream-note {
  font-size: var(--lv-text-xs);
}

.stream-note--warn {
  color: var(--lv-warning, #b45309);
}

.stream-note--ok {
  color: var(--lv-good, #15803d);
}
</style>
