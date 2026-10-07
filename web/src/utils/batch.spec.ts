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
  it('maps each store state to its own colour', () => {
    expect(batchTagType('done')).toBe('success')
    expect(batchTagType('failed')).toBe('danger')
    expect(batchTagType('running')).toBe('primary')
    expect(batchTagType('pending')).toBe('info')
  })

  it('keeps "interrupted" distinct from "failed"', () => {
    // Interrupted means the run stopped before the batch reached a verdict.
    // Red would tell the operator the change broke when nothing was decided.
    expect(batchTagType('interrupted')).toBe('warning')
    expect(batchTagType('interrupted')).not.toBe(batchTagType('failed'))
  })

  it('does not treat the word "success" as a completed batch', () => {
    // Regression: the monitor used to compare against a `success` state that
    // nothing ever writes. An unknown value must read neutral, never green.
    expect(batchTagType('success')).toBe('info')
    expect(batchTagType('whatever-the-backend-adds-next')).toBe('info')
  })
})

describe('batchBarStatus', () => {
  it('mirrors the tag mapping for the bar widget', () => {
    expect(batchBarStatus('done')).toBe('success')
    expect(batchBarStatus('failed')).toBe('exception')
    expect(batchBarStatus('interrupted')).toBe('warning')
  })

  it('leaves in-flight and unknown states without an asserted outcome', () => {
    expect(batchBarStatus('running')).toBeUndefined()
    expect(batchBarStatus('pending')).toBeUndefined()
    expect(batchBarStatus('success')).toBeUndefined()
  })
})
