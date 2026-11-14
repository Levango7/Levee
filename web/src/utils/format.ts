// Shared UI helpers: status colors, formatters, etc. Kept framework-agnostic
// so they can be unit-tested without mounting a component.
import dayjs from 'dayjs'
import type { ChangeStatus, PlanState, Priority, ApprovalStatus, ExecutionVerdict, RollbackStrategy } from '@/types/levee'

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

// PRIORITIES is the runtime form of the `Priority` union, which cannot be
// iterated. It used to be a hand-copy here (a third spelling beside the union and
// the two tables above, after the instantiate wizard's four inline <el-option>
// lines). It is now re-exported from the generated vocabulary, whose own source is
// internal/state.PriorityNames() — so the accepted set, the dropdown order and the
// Go error text come from one place and cannot disagree.
export { PRIORITIES } from '@/types/vocabulary'

// priorityLabel renders a priority for the Chinese console. Both fallbacks are
// deliberate:
//   - an EMPTY value renders 未记录 rather than nothing. runs.priority is '' on every
//     row created before schema v9 whose value could not be attributed — "high" is the
//     one spelling the priority and the approval-tier vocabularies share, and the v9
//     backfill left it in the tier column instead of guessing (internal/state/
//     priority.go). An empty cell would read as "this change claimed nothing", which
//     is a claim we cannot make from the data we hold.
//   - a spelling outside the vocabulary stays verbatim, the way nodeStatusLabel
//     behaves. Such rows exist only from before entrypoint validation; printing a
//     friendlier word would hide what the server is actually storing.
export function priorityLabel(priority: string): string {
  if (!priority) return '未记录'
  return PRIORITY_LABEL[priority as Priority] ?? priority
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

// Remediable statuses: the two partial-rollback verdicts. Here the outstanding
// work is to FINISH compensating the hosts that did not roll back — a different
// action from retry, which re-drives the forward change. Both are offered and
// deliberately not merged into one button: on these two states the operator has
// two different intents, and the UI must not pick for them.
//
// Backed by POST /changes/{id}/rollback (idempotent) — the same endpoint takes
// dry_run, and that is what lets the dialog show the evidence (which hosts
// would be compensated, which skipped) before anything moves.
const REMEDIABLE_STATUSES: ReadonlySet<ChangeStatus> = new Set([
  'rolled_back_partial',
  'rollback_incomplete',
])

export function isRemediableStatus(status: ChangeStatus): boolean {
  return REMEDIABLE_STATUSES.has(status)
}

// The four action gates below mirror the server's own switches
// (internal/grpc/change_service.go): a button is offered exactly where the RPC
// would accept it, so the UI stops promising actions the server refuses. The
// admission sets are written out rather than derived, because the server's
// rules are stated as exclusions in one place and inclusions in another:
//
//   - cancel: isValidTransition's "cancelled" arm — everything EXCEPT the
//     endpoints that already recorded a verdict (completed, cancelled,
//     archived) and interrupted, which is a takeover terminal: cancelling
//     afterwards would rewrite history the audit chain already recorded.
//   - archive: ArchiveChange refuses only running/paused (they still own live
//     execution state); an archived change is not offered again.
//   - pause/resume: isValidTransition's "paused"/"running" arms.
const CANCELLABLE_STATUSES: ReadonlySet<ChangeStatus> = new Set([
  'draft',
  'planned',
  'pending',
  'approved',
  'rejected',
  'running',
  'paused',
  'failed',
  'rolled_back',
  'rolled_back_partial',
  'rollback_incomplete',
])

const ARCHIVABLE_STATUSES: ReadonlySet<ChangeStatus> = new Set([
  'draft',
  'planned',
  'pending',
  'approved',
  'rejected',
  'completed',
  'failed',
  'cancelled',
  'rolled_back',
  'rolled_back_partial',
  'rollback_incomplete',
  'interrupted',
])

const PAUSABLE_STATUSES: ReadonlySet<ChangeStatus> = new Set(['running', 'pending'])

export function isCancellableStatus(status: ChangeStatus): boolean {
  return CANCELLABLE_STATUSES.has(status)
}

export function isArchivableStatus(status: ChangeStatus): boolean {
  return ARCHIVABLE_STATUSES.has(status)
}

export function isPausableStatus(status: ChangeStatus): boolean {
  return PAUSABLE_STATUSES.has(status)
}

export function isResumableStatus(status: ChangeStatus): boolean {
  return status === 'paused'
}

export function formatTimestamp(ts: string | number | undefined | null): string {
  if (!ts) return '-'
  const n = typeof ts === 'string' ? parseInt(ts, 10) : ts
  if (isNaN(n)) return '-'
  return dayjs.unix(n).format('YYYY-MM-DD HH:mm:ss')
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

// ============================================================
// DB Change Ticket (全链视图) 格式化工具
// ============================================================

/** 计划三态标签 */
export function planStateLabel(state: PlanState): string {
  const map: Record<PlanState, string> = {
    none: '无计划',
    readable: '可读',
    unreadable: '不可读',
  }
  return map[state] ?? state
}

/** 审批状态标签 — approval.Status vocabulary */
export function approvalStatusLabel(status: ApprovalStatus): string {
  const map: Record<ApprovalStatus, string> = {
    pending: '进行中',
    approved: '已通过',
    rejected: '已拒绝',
    expired: '已过期',
  }
  return map[status] ?? status
}


/** 执行裁决标签 — evidence.Kind vocabulary */
export function executionVerdictLabel(verdict: ExecutionVerdict): string {
  const map: Record<ExecutionVerdict, string> = {
    not_started: '未开始',
    in_flight: '执行中',
    complete: '完成',
    partial: '部分完成',
    missing: '证据缺失',
    unrecorded: '无记录',
  }
  return map[verdict] ?? verdict
}

/** 回滚策略标签 — dsl.RollbackStrategy vocabulary */
export function rollbackStrategyLabel(strategy: RollbackStrategy): string {
  const map: Record<RollbackStrategy, string> = {
    snapshot: '快照',
    'undo-action': '反向动作',
    'config-revert': '配置还原',
  }
  return map[strategy] ?? strategy
}
