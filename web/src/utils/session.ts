// Conversation session-state labels.
//
// The wire value is not a string constant set: `SessionState` is an int enum and
// the REST/JSON surface carries whatever `SessionState.String()` returns
// (internal/conversation/session.go:52-70). That makes the mirror easy to get wrong
// in two ways — a new enum member nobody mapped, and the `default:` branch of
// String() ("unknown") being mistaken for a state the machine can be in. Both are
// now pinned in the owner package: internal/conversation/vocabulary.go derives the
// names from String() itself (so there is no second list to forget to update), and
// vocabulary_ownership_test.go checks the enum's upper bound and that no two states
// share a spelling. The list below is generated from that data by internal/docgen.

import type { SessionStateName } from '@/types/vocabulary'
import { SESSION_STATES } from '@/types/vocabulary'

// Re-exported, not re-declared. `unknown` is in the vocabulary because a row whose
// value is out of range reaches the console as that word — it is still not a state
// the state machine transitions into, which is why the label below says 未知 rather
// than naming a phase.
export { SESSION_STATES }
export type { SessionStateName }

const LABEL_BY_STATE: Record<SessionStateName, string> = {
  idle: '等待输入',
  diagnosing: '诊断中',
  recommending: '生成建议中',
  reviewing: '等待确认',
  executing: '执行中',
  done: '已完成',
  failed: '已失败',
  unknown: '未知',
}

/**
 * Chinese label for a conversation session state.
 *
 * `reviewing` reads 等待确认 rather than 审核中: the engine is blocked on a human
 * approving or editing the proposed fix (session.go:41-43), so naming the actor
 * matters more than naming the phase.
 */
export function sessionStateLabel(state: string): string {
  return LABEL_BY_STATE[state as SessionStateName] ?? state
}

export type SessionTagTone = 'success' | 'danger' | 'warning' | 'info' | 'primary'

/**
 * Tag colour per session state.
 *
 * This used to be an inline ternary in ConversationView.vue that only knew
 * `failed` and `done`; the other six states all drew grey, including
 * `reviewing`, which is precisely the state where the engine is waiting on a
 * human. Same defect shape as the batch rows in #92: the view carried its own
 * half-copy of the vocabulary while a tested table existed next to it.
 */
const TONE_BY_STATE: Record<SessionStateName, SessionTagTone> = {
  idle: 'info',
  diagnosing: 'primary',
  recommending: 'primary',
  reviewing: 'warning',
  executing: 'primary',
  done: 'success',
  failed: 'danger',
  unknown: 'info',
}

/** Tag colour for a session state. Unknown states fall back to `info`, never to a colour that asserts an outcome. */
export function sessionTagType(state: string): SessionTagTone {
  return TONE_BY_STATE[state as SessionStateName] ?? 'info'
}
