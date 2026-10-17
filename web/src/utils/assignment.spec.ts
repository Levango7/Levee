import { describe, expect, it } from 'vitest'

import { ASSIGNMENT_STATES as ASSIGNMENT_STATES_FROM_OWNER } from '@/types/vocabulary'

import { batchLabel } from './batch'
import { ASSIGNMENT_STATES, assignmentLabel } from './assignment'

// The vocabulary under test is internal/state's assignment-state group, not the batch
// one — the two share three spellings (pending / done / interrupted) and mean
// different things, which is why they are two generated tables with two type names.
//
// Members are no longer written down here: ./assignment.ts re-exports the array
// internal/docgen generates from state.AssignmentStateNames(). Keeping the group
// distinction honest is Go's job (vocabulary_ownership_test.go pins each list to its
// own constant group, and TestAssignmentListExcludesTheResultGroup refuses the result
// range); this case proves the wiring, which a restated literal list could not.
describe('ASSIGNMENT_STATES', () => {
  it('arrives from the generated vocabulary instead of a copy in ./assignment.ts', () => {
    expect(ASSIGNMENT_STATES).toBe(ASSIGNMENT_STATES_FROM_OWNER)
    expect(ASSIGNMENT_STATES.length).toBeGreaterThan(0)
    expect(new Set(ASSIGNMENT_STATES).size).toBe(ASSIGNMENT_STATES.length)
  })

  it('names every declared state in Chinese', () => {
    for (const state of ASSIGNMENT_STATES) {
      const label = assignmentLabel(state)
      expect(label).not.toBe('')
      expect(label).not.toBe(state)
    }
  })
})

describe('assignmentLabel', () => {
  it('maps the states the dispatcher writes', () => {
    expect(assignmentLabel('pending')).toBe('待领取')
    expect(assignmentLabel('executing')).toBe('执行中')
    expect(assignmentLabel('done')).toBe('已完成')
    expect(assignmentLabel('interrupted')).toBe('已中断')
  })

  it('does not read a waiting assignment as a waiting batch', () => {
    // `pending` on this row means "not yet claimed by a worker"; on a batch it
    // means "not yet run". Someone collapsing the two tables onto batchLabel is
    // the plausible shortcut here, and it would silently tell operators that
    // unclaimed work is unstarted work.
    expect(assignmentLabel('pending')).not.toBe(batchLabel('pending'))
  })

  it('reads terminal states apart from each other', () => {
    expect(assignmentLabel('done')).not.toBe(assignmentLabel('interrupted'))
  })

  it('shows an unrecognised wire value verbatim instead of guessing', () => {
    expect(assignmentLabel('leased')).toBe('leased')
  })
})
