// Assignment-state mapping for the cluster page's "分配状态分布" panel.
//
// Same reasoning as ./batch.ts, for a different column: these rows show counts of
// assignments grouped by `assignments.state`, and the page's chrome is Chinese
// while the value rendered was the wire string. The repo has no component-test
// dependency, so the mapping lives here where vitest can reach it.
//
// Do NOT reuse batch.ts. The two vocabularies overlap in spelling but not in
// meaning — `pending`, `done` and `interrupted` exist on both sides, and on the
// batch side `done` is a legacy spelling that counts as complete while on the
// assignment side `done` is the live terminal state. `internal/state` keeps them in
// separate order lists (batchStateOrder vs assignmentStateOrder) and generates two
// separate tables, so a change to one vocabulary cannot borrow the other's label
// table. internal/state/vocabulary_ownership_test.go is what keeps each list equal
// to its own constant group.

import type { AssignmentState } from '@/types/vocabulary'
import { ASSIGNMENT_STATES } from '@/types/vocabulary'

// Generated from internal/state.AssignmentStateNames() and re-exported; see
// ./batch.ts for why this is an import + export pair rather than `export … from`.
export { ASSIGNMENT_STATES }
export type { AssignmentState }

const LABEL_BY_STATE: Record<AssignmentState, string> = {
  pending: '待领取',
  executing: '执行中',
  done: '已完成',
  interrupted: '已中断',
}

/**
 * Chinese label for an assignment state.
 *
 * Unrecognised values pass through verbatim: a count row we cannot name is a
 * signal worth seeing, whereas inventing a translation would let a new backend
 * state masquerade as a known one. `待领取` rather than `待执行` because this row
 * counts assignments waiting to be claimed by a worker, not batches waiting to run.
 */
export function assignmentLabel(state: string): string {
  return LABEL_BY_STATE[state as AssignmentState] ?? state
}
