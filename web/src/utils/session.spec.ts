import { describe, expect, it } from 'vitest'

import { SESSION_STATES, sessionStateLabel, sessionTagType } from './session'

// The wire value is whatever SessionState.String() returns
// (internal/conversation/session.go:52-70). The Go half of this pin is
// internal/conversation/session_state_vocabulary_test.go, which parses those
// return statements rather than restating them.

describe('SESSION_STATES', () => {
  it('is exactly what SessionState.String() can emit', () => {
    expect([...SESSION_STATES].sort()).toEqual(
      ['diagnosing', 'done', 'executing', 'failed', 'idle', 'recommending', 'reviewing', 'unknown'].sort(),
    )
  })

  it('names every declared state in Chinese', () => {
    for (const state of SESSION_STATES) {
      const label = sessionStateLabel(state)
      expect(label).not.toBe('')
      expect(label).not.toBe(state)
    }
  })
})

describe('sessionStateLabel', () => {
  it('names the human-in-the-loop state by who is holding it', () => {
    // StateReviewing means the engine is waiting for a person to approve or edit
    // the proposal (session.go:41-43). Calling it 审核中 hides that the operator
    // is the blocker, which is the one thing that page needs to say.
    expect(sessionStateLabel('reviewing')).toBe('等待确认')
    expect(sessionStateLabel('idle')).toBe('等待输入')
  })

  it('keeps the machine phases apart', () => {
    expect(sessionStateLabel('diagnosing')).not.toBe(sessionStateLabel('recommending'))
    expect(sessionStateLabel('executing')).not.toBe(sessionStateLabel('done'))
  })

  it('shows an unrecognised wire value verbatim instead of guessing', () => {
    expect(sessionStateLabel('paused')).toBe('paused')
  })
})

describe('sessionTagType', () => {
  it('gives every declared state an explicit colour', () => {
    // Regression: the view used a ternary that only knew failed/done, so six
    // states — including the one waiting on a human — all drew grey.
    const explicit = SESSION_STATES.filter(s => sessionTagType(s) !== 'info')
    expect(explicit.length).toBeGreaterThanOrEqual(5)
  })

  it('colors the blocked-on-human state distinctly from a completed one', () => {
    expect(sessionTagType('reviewing')).toBe('warning')
    expect(sessionTagType('reviewing')).not.toBe(sessionTagType('done'))
    expect(sessionTagType('failed')).toBe('danger')
    expect(sessionTagType('done')).toBe('success')
  })

  it('keeps an unknown state neutral', () => {
    expect(sessionTagType('unknown')).toBe('info')
    expect(sessionTagType('whatever-next')).toBe('info')
  })
})
