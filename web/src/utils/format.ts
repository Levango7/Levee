// Shared UI helpers: status colors, formatters, etc. Kept framework-agnostic
// so they can be unit-tested without mounting a component.
import dayjs from 'dayjs'
import type { ChangeStatus, Priority } from '@/types/levee'

export const STATUS_LABEL: Record<ChangeStatus, string> = {
  planned: '已计划',
  pending_approval: '待审批',
  approved: '已审批',
  running: '执行中',
  paused: '已暂停',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
  rolled_back: '已回滚',
  archived: '已归档',
}

export const STATUS_COLOR: Record<ChangeStatus, string> = {
  planned: 'info',
  pending_approval: 'warning',
  approved: 'primary',
  running: 'primary',
  paused: 'warning',
  completed: 'success',
  failed: 'danger',
  cancelled: 'info',
  rolled_back: 'info',
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