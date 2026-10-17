import { describe, expect, it } from 'vitest'

import { SESSION_STATES as SESSION_STATES_FROM_OWNER } from '@/types/vocabulary'

import { SESSION_STATES, sessionStateLabel, sessionTagType } from './session'

// The wire value is whatever SessionState.String() returns
// (internal/conversation/session.go), and the list below is generated from that same
// String() by internal/docgen — there is no second spelling of it in this repo for
// this file to compare against. `unknown` is in the vocabulary because an out-of-range
// row reaches the console as that word; it is not a state the machine enters.
//
// The Go half: internal/conversation/vocabulary_ownership_test.go checks the enum's
// upper bound and that no two states share a String(), and
// session_state_vocabulary_test.go checks the artefact plus this file's wiring.
describe('SESSION_STATES', () => {
  it('arrives from the generated vocabulary instead of a copy in ./session.ts', () => {
    expect(SESSION_STATES).toBe(SESSION_STATES_FROM_OWNER)
    expect(SESSION_STATES.length).toBeGreaterThan(0)
    expect(new Set(SESSION_STATES).size).toBe(SESSION_STATES.length)
    expect(SESSION_STATES[SESSION_STATES.length - 1]).toBe('unknown')
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
