// Batch progress mapping for the monitor page.
//
// These live here rather than inside MonitorView.vue for one reason: they encode
// claims about server state ("this batch is finished", "this batch was
// interrupted, not failed"), and a claim like that has to be testable without a
// browser. The repo has no component-test dependency (@vue/test-utils is not in
// devDependencies), so the logic the page renders is separated from the markup
// that renders it.
//
// The vocabulary under test is what the WRITER emits, not what a constant list
// suggests: internal/wiring/persist.go writes `completed` / `failed`, and the
// rollback path writes `rolled_back`. internal/state declares older spellings
// (`done` / `interrupted` / `pending` / `running`) that nothing writes today,
// and that gap is precisely the bug this file was born from — state counted
// `done`, the engine wrote `completed`, so every finished run reported
// "0 batches done". Mapping keys must be checked against the writer, and the
// backend seam is pinned by internal/wiring/batch_summary_seam_test.go.

import type { BatchProgressDTO } from '@/api'

// The seven spellings a batch row can carry. This list, and not a switch
// statement, is the source of truth below: keyed `Record`s make "no label / no
// colour for the new state" a compile error instead of a silent fallback, which
// is how a finished batch came to be drawn grey with the English word on it.
// internal/state/batch_status_vocabulary_test.go pins this list against
// internal/state/store.go's constants, so the Go side cannot add a spelling
// without turning a test red here.
export const BATCH_STATES = ['pending', 'running', 'completed', 'done', 'failed', 'rolled_back', 'interrupted'] as const
export type BatchState = (typeof BATCH_STATES)[number]

export type TagTone = 'success' | 'danger' | 'warning' | 'info' | 'primary'
export type BarStatus = 'success' | 'exception' | 'warning' | undefined

/**
 * Fraction of assigned hosts that have been accounted for.
 *
 * Deliberately not a time estimate: 100% means every host in the batch has a
 * result, and a batch that never started reads 0. A zero-host batch returns 0
 * rather than NaN — an empty batch is a real shape (a target set that filtered
 * down to nothing), and `NaN%` in a progress bar is a bug report waiting.
 */
export function batchProgress(b: Pick<BatchProgressDTO, 'total_hosts' | 'succeeded' | 'failed'>): number {
  if (b.total_hosts <= 0) return 0
  const accounted = b.succeeded + b.failed
  // Clamped: a malformed row where succeeded+failed exceeds total_hosts must not
  // push the bar past 100%, because that reads as "the engine counted hosts twice"
  // — a real defect worth surfacing, not silently drawing.
  return Math.min(100, Math.round((accounted / b.total_hosts) * 100))
}

/** Tag colour for a batch state. Unknown states fall back to `info`, never to a colour that asserts an outcome.
 *
 * The keys here are the strings the backend actually writes — see
 * internal/wiring/persist.go: `completed` / `failed` / `rolled_back`.
 * `done` is accepted too because internal/state counts both spellings as
 * complete, and a grey bar next to "this batch is done" would contradict the
 * summary on the same page.
 */
const TAG_TONE_BY_STATE: Record<BatchState, TagTone> = {
  completed: 'success',
  done: 'success',
  failed: 'danger',
  rolled_back: 'warning',
  interrupted: 'warning',
  running: 'primary',
  pending: 'info',
}

export function batchTagType(status: string): TagTone {
  return TAG_TONE_BY_STATE[status as BatchState] ?? 'info'
}

/** Progress-bar status for a batch state; `undefined` keeps the bar neutral. */
const BAR_STATUS_BY_STATE: Record<BatchState, BarStatus> = {
  completed: 'success',
  done: 'success',
  failed: 'exception',
  rolled_back: 'warning',
  interrupted: 'warning',
  running: undefined,
  pending: undefined,
}

export function batchBarStatus(status: string): BarStatus {
  return BAR_STATUS_BY_STATE[status as BatchState]
}

const LABEL_BY_STATE: Record<BatchState, string> = {
  completed: '已完成',
  done: '已完成',
  failed: '失败',
  rolled_back: '已回滚',
  interrupted: '已中断',
  running: '执行中',
  pending: '待执行',
}

/**
 * Chinese label for a batch state, for pages whose chrome is Chinese.
 *
 * The fallback is deliberately the raw wire value, not a translation of it: an
 * unrecognised spelling must stay visible so an operator can report it, and
 * guessing ("some new word — show 已完成") would assert an outcome the engine
 * never wrote. Until this existed both batch rows rendered the raw string, so
 * the monitor page read "completed" inside an otherwise Chinese table while its
 * neighbour column said 已完成.
 */
export function batchLabel(status: string): string {
  return LABEL_BY_STATE[status as BatchState] ?? status
}
