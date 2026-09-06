// Unit tests for the framework-agnostic format helpers. TZ is pinned to UTC
// before dayjs is loaded so formatTimestamp assertions are deterministic on
// every machine (dayjs formats through the host timezone otherwise).
process.env.TZ = 'UTC'

import { describe, expect, it } from 'vitest'

import {
  PRIORITY_COLOR,
  PRIORITY_LABEL,
  STATUS_COLOR,
  STATUS_LABEL,
  formatDuration,
  formatTimestamp,
  formatUptime,
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

  it('priority tables agree on keys and normal has the empty (default) color', () => {
    expect(Object.keys(PRIORITY_LABEL).sort()).toEqual(Object.keys(PRIORITY_COLOR).sort())
    expect(PRIORITY_COLOR.normal).toBe('')
    expect(PRIORITY_LABEL.urgent).toBe('紧急')
  })
})
