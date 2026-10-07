import { describe, expect, it } from 'vitest'
import { batchBarStatus, batchProgress, batchTagType } from './batch'

// The vocabulary under test is internal/state/store.go's, not the UI's wish:
// pending | running | done | failed | interrupted.

describe('batchProgress', () => {
  it('is the share of assigned hosts that have an outcome', () => {
    expect(batchProgress({ total_hosts: 10, succeeded: 5, failed: 0 })).toBe(50)
    expect(batchProgress({ total_hosts: 10, succeeded: 7, failed: 3 })).toBe(100)
    expect(batchProgress({ total_hosts: 4, succeeded: 1, failed: 0 })).toBe(25)
  })

  it('gives an unstarted batch zero, not a fabricated mid-point', () => {
    expect(batchProgress({ total_hosts: 8, succeeded: 0, failed: 0 })).toBe(0)
  })

  it('returns 0 for a zero-host batch instead of NaN', () => {
    // A batch with no hosts is a real shape (a target set that filtered down to
    // nothing). NaN% in a progress bar is not "empty", it is a broken widget.
    expect(batchProgress({ total_hosts: 0, succeeded: 0, failed: 0 })).toBe(0)
  })

  it('clamps rather than drawing past 100%', () => {
    expect(batchProgress({ total_hosts: 2, succeeded: 3, failed: 0 })).toBe(100)
  })
})

describe('batchTagType', () => {
  it('colours the values the writer actually emits', () => {
    // Regression: the engine writes `completed` (internal/wiring/persist.go),
    // while this page originally keyed on `success` and then on `done`. Neither
    // is ever written, so a finished batch drew a grey bar.
    expect(batchTagType('completed')).toBe('success')
    expect(batchTagType('failed')).toBe('danger')
    expect(batchTagType('running')).toBe('primary')
    expect(batchTagType('pending')).toBe('info')
  })

  it('also colours the legacy `done` spelling, which state counts as complete', () => {
    expect(batchTagType('done')).toBe('success')
  })

  it('keeps "rolled_back" and "interrupted" distinct from "failed"', () => {
    // Neither means "this batch broke while running": rolled_back is a rollback
    // outcome, interrupted is "the run stopped before a verdict". Red on either
    // would tell the operator something the engine never decided.
    expect(batchTagType('rolled_back')).toBe('warning')
    expect(batchTagType('interrupted')).toBe('warning')
    expect(batchTagType('rolled_back')).not.toBe(batchTagType('failed'))
  })

  it('does not treat an unknown word as a completed batch', () => {
    // Any spelling the backend adds must default to neutral, never green.
    expect(batchTagType('success')).toBe('info')
    expect(batchTagType('whatever-the-backend-adds-next')).toBe('info')
  })
})

describe('batchBarStatus', () => {
  it('mirrors the tag mapping for the bar widget', () => {
    expect(batchBarStatus('completed')).toBe('success')
    expect(batchBarStatus('done')).toBe('success')
    expect(batchBarStatus('failed')).toBe('exception')
    expect(batchBarStatus('rolled_back')).toBe('warning')
    expect(batchBarStatus('interrupted')).toBe('warning')
  })

  it('leaves in-flight and unknown states without an asserted outcome', () => {
    expect(batchBarStatus('running')).toBeUndefined()
    expect(batchBarStatus('pending')).toBeUndefined()
    expect(batchBarStatus('success')).toBeUndefined()
  })
})
