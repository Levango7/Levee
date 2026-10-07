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
export function batchTagType(status: string): TagTone {
  switch (status) {
    case 'completed':
    case 'done':
      return 'success'
    case 'failed':
      return 'danger'
    case 'rolled_back':
    case 'interrupted':
      return 'warning'
    case 'running':
      return 'primary'
    default:
      return 'info'
  }
}

/** Progress-bar status for a batch state; `undefined` keeps the bar neutral. */
export function batchBarStatus(status: string): BarStatus {
  switch (status) {
    case 'completed':
    case 'done':
      return 'success'
    case 'failed':
      return 'exception'
    case 'rolled_back':
    case 'interrupted':
      return 'warning'
    default:
      return undefined
  }
}
