import { describe, expect, it } from 'vitest'

import { batchLabel } from './batch'
import { ASSIGNMENT_STATES, assignmentLabel } from './assignment'

// The vocabulary under test is internal/state's assignment-state group
// (store.go:738-741), not the batch one — the two share three spellings
// (pending / done / interrupted) and mean different things.

describe('ASSIGNMENT_STATES', () => {
  it('is exactly the four states internal/state declares', () => {
    expect([...ASSIGNMENT_STATES].sort()).toEqual(
      ['done', 'executing', 'interrupted', 'pending'].sort(),
    )
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
