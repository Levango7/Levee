// Shared UI helpers: status colors, formatters, etc. Kept framework-agnostic
// so they can be unit-tested without mounting a component.
import dayjs from 'dayjs'
import type { ChangeStatus, Priority } from '@/types/levee'

// Status vocabulary mirrors the backend state machine in
// internal/grpc/change_service.go (isValidTransition / terminalRunStatuses),
// which is also what proto/levee.proto documents. `pending_approval` used to
// live here as a second "待审批" key: the backend never emitted it, so every
// query and v-if keyed on it silently matched nothing.
export const STATUS_LABEL: Record<ChangeStatus, string> = {
  draft: '草稿',
  planned: '已计划',
  pending: '待审批',
  approved: '已审批',
  rejected: '已拒绝',
  running: '执行中',
  paused: '已暂停',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
  rolled_back: '已回滚',
  rolled_back_partial: '部分回滚',
  rollback_incomplete: '回滚未完成',
  interrupted: '已中断',
  archived: '已归档',
}

export const STATUS_COLOR: Record<ChangeStatus, string> = {
  draft: 'info',
  planned: 'info',
  pending: 'warning',
  approved: 'primary',
  rejected: 'danger',
  running: 'primary',
  paused: 'warning',
  completed: 'success',
  failed: 'danger',
  cancelled: 'info',
  rolled_back: 'info',
  rolled_back_partial: 'warning',
  rollback_incomplete: 'danger',
  interrupted: 'danger',
  archived: 'info',
}

export const PRIORITY_LABEL: Record<Priority, string> = {
  low: '低',
  normal: '中',
  high: '高',
  urgent: '紧急',
}

export const PRIORITY_COLOR: Record<Priority, string> = {
  low: 'info',
  normal: '',
  high: 'warning',
  urgent: 'danger',
}

// Retryable statuses mirror the backend RetryChange admission set
// (change_service.go): failed, rolled_back, interrupted — plus the two D-2 v2
// rollback verdicts, which are failure-family terminals whose re-drive entry
// point is also RetryChange. interrupted is the cluster takeover terminal
// whose machine re-drive entry point IS retry. Keep in sync with the gRPC
// guard when the admission set changes.
const RETRYABLE_STATUSES: ReadonlySet<ChangeStatus> = new Set([
  'failed',
  'rolled_back',
  'rolled_back_partial',
  'rollback_incomplete',
  'interrupted',
])

export function isRetryableStatus(status: ChangeStatus): boolean {
  return RETRYABLE_STATUSES.has(status)
}

export function formatTimestamp(ts: number): string {
  if (!ts) return '-'
  return dayjs.unix(ts).format('YYYY-MM-DD HH:mm:ss')
}

export function formatDuration(ms: number): string {
  if (!ms || ms < 0) return '-'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  if (ms < 3_600_000) return `${(ms / 60_000).toFixed(1)} min`
  return `${(ms / 3_600_000).toFixed(1)} h`
}

export function formatUptime(seconds: number): string {
  if (!seconds || seconds < 0) return '-'
  const d = Math.floor(seconds / 86_400)
  const h = Math.floor((seconds % 86_400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const parts: string[] = []
  if (d > 0) parts.push(`${d}d`)
  if (h > 0) parts.push(`${h}h`)
  parts.push(`${m}m`)
  return parts.join(' ')
}