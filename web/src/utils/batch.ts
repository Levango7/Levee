// Batch progress mapping for the monitor page.
//
// These live here rather than inside MonitorView.vue for one reason: they encode
// claims about server state ("this batch is finished", "this batch was
// interrupted, not failed"), and a claim like that has to be testable without a
// browser. The repo has no component-test dependency (@vue/test-utils is not in
// devDependencies), so the logic the page renders is separated from the markup
// that renders it.
//
// The status vocabulary is the store's own — internal/state/store.go
// (BatchStatePending/Running/Done/Failed/Interrupted). Two things follow, and
// both are regression points this file exists to hold:
//   - there is NO `success` state; a batch completes as `done`. The monitor used
//     to compare against `success`, so a finished batch rendered as a neutral
//     bar no matter what the engine said.
//   - `interrupted` is not a failure. It means the run stopped before the batch
//     reached a verdict; painting it red would tell an operator the change broke.

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

/** Tag colour for a batch state. Unknown states fall back to `info`, never to a colour that asserts an outcome. */
export function batchTagType(status: string): TagTone {
  switch (status) {
    case 'done':
      return 'success'
    case 'failed':
      return 'danger'
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
    case 'done':
      return 'success'
    case 'failed':
      return 'exception'
    case 'interrupted':
      return 'warning'
    default:
      return undefined
  }
}
