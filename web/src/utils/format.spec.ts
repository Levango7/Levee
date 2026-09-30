// Unit tests for the framework-agnostic format helpers. TZ is pinned to UTC
// before dayjs is loaded so formatTimestamp assertions are deterministic on
// every machine (dayjs formats through the host timezone otherwise).
process.env.TZ = 'UTC'

import { describe, expect, it } from 'vitest'

import type { ChangeStatus } from '@/types/levee'
import {
  PRIORITY_COLOR,
  PRIORITY_LABEL,
  STATUS_COLOR,
  STATUS_LABEL,
  formatDuration,
  formatTimestamp,
  formatUptime,
  isRetryableStatus,
} from './format'

describe('formatTimestamp', () => {
  it('renders zero/invalid as a dash', () => {
    expect(formatTimestamp(0)).toBe('-')
    expect(formatTimestamp(NaN)).toBe('-')
  })

  it('formats unix seconds as YYYY-MM-DD HH:mm:ss in UTC', () => {
    const ts = Date.UTC(2026, 8, 6, 12, 30, 0) / 1000
    expect(formatTimestamp(ts)).toBe('2026-09-06 12:30:00')
  })
})

describe('formatDuration', () => {
  it('rejects non-positive durations', () => {
    expect(formatDuration(0)).toBe('-')
    expect(formatDuration(-5)).toBe('-')
  })

  it('scales the unit to the magnitude', () => {
    expect(formatDuration(999)).toBe('999 ms')
    expect(formatDuration(1000)).toBe('1.0 s')
    expect(formatDuration(59_999)).toBe('60.0 s')
    expect(formatDuration(60_000)).toBe('1.0 min')
    expect(formatDuration(90_000)).toBe('1.5 min')
    expect(formatDuration(3_600_000)).toBe('1.0 h')
    expect(formatDuration(5_400_000)).toBe('1.5 h')
  })
})

describe('formatUptime', () => {
  it('rejects non-positive durations', () => {
    expect(formatUptime(0)).toBe('-')
    expect(formatUptime(-1)).toBe('-')
  })

  it('emits d/h/m parts, always with minutes', () => {
    expect(formatUptime(90)).toBe('1m')
    expect(formatUptime(3_661)).toBe('1h 1m')
    expect(formatUptime(90_061)).toBe('1d 1h 1m')
    expect(formatUptime(120)).toBe('2m')
  })
})

describe('label/color tables', () => {
  it('every status has a label and a color', () => {
    for (const key of Object.keys(STATUS_LABEL)) {
      expect(STATUS_COLOR[key as keyof typeof STATUS_COLOR]).toBeTruthy()
    }
    expect(STATUS_LABEL.rolled_back).toBe('已回滚')
    expect(STATUS_LABEL.cancelled).toBe('已取消')
  })

  it('covers the D-2 v2 rollback verdicts, which are not a clean rollback', () => {
    // A partial/incomplete rollback must read differently from rolled_back:
    // the state was NOT restored, so presenting it as 已回滚 would repeat the
    // very misreport D-2 was built to stop.
    expect(STATUS_LABEL.rolled_back_partial).toBe('部分回滚')
    expect(STATUS_LABEL.rollback_incomplete).toBe('回滚未完成')
    expect(STATUS_LABEL.rolled_back_partial).not.toBe(STATUS_LABEL.rolled_back)
    expect(STATUS_LABEL.rollback_incomplete).not.toBe(STATUS_LABEL.rolled_back)
    expect(STATUS_COLOR.rollback_incomplete).toBe('danger')
  })

  it('carries no status the backend never emits', () => {
    // `pending_approval` used to sit in this table as a second 待审批 key
    // beside `pending`. The server never returned it, so the approval list
    // query and the mobile approve button keyed on it matched nothing. If a
    // value comes back, the vocabulary has drifted from the backend again.
    expect(Object.keys(STATUS_LABEL)).not.toContain('pending_approval')
    expect(Object.keys(STATUS_LABEL)).toContain('pending')
  })

  it('the label and color tables agree on the key set', () => {
    expect(Object.keys(STATUS_LABEL).sort()).toEqual(Object.keys(STATUS_COLOR).sort())
  })

  it('priority tables agree on keys and normal has the empty (default) color', () => {
    expect(Object.keys(PRIORITY_LABEL).sort()).toEqual(Object.keys(PRIORITY_COLOR).sort())
    expect(PRIORITY_COLOR.normal).toBe('')
    expect(PRIORITY_LABEL.urgent).toBe('紧急')
  })
})

describe('isRetryableStatus', () => {
  // Mirrors the backend RetryChange admission set (change_service.go):
  // failed / rolled_back / interrupted, plus the D-2 v2 rollback verdicts
  // (failure-family terminals whose re-drive entry point is also retry).
  it('admits the retryable terminals', () => {
    expect(isRetryableStatus('failed')).toBe(true)
    expect(isRetryableStatus('rolled_back')).toBe(true)
    expect(isRetryableStatus('rolled_back_partial')).toBe(true)
    expect(isRetryableStatus('rollback_incomplete')).toBe(true)
    expect(isRetryableStatus('interrupted')).toBe(true)
  })

  it('refuses every non-retryable state', () => {
    const retryable = [
      'failed',
      'rolled_back',
      'rolled_back_partial',
      'rollback_incomplete',
      'interrupted',
    ]
    const others = Object.keys(STATUS_LABEL).filter((s) => !retryable.includes(s))
    // Guard against vocabulary drift: the non-retryable set must be
    // exactly the complement, so a newly added status cannot silently
    // default into retryable (it must explicitly join one set).
    expect(others.length).toBeGreaterThan(5)
    for (const s of others) {
      expect(isRetryableStatus(s as ChangeStatus)).toBe(false)
    }
  })
})
