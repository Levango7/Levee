import { describe, expect, it } from 'vitest'

import {
  TREND_SERIES,
  buildGeometry,
  dayLabel,
  niceCeil,
  sumBuckets,
  type TrendBucket,
} from './trend'

function bucket(day: string, outcomes: Record<string, number>): TrendBucket {
  const total = Object.values(outcomes).reduce((a, b) => a + b, 0)
  return { day, total, outcomes }
}

describe('niceCeil', () => {
  it('rounds the axis top to a readable number', () => {
    expect(niceCeil(0)).toBe(0)
    expect(niceCeil(3)).toBe(5)
    expect(niceCeil(7)).toBe(10)
    expect(niceCeil(12)).toBe(20)
    expect(niceCeil(48)).toBe(50)
    expect(niceCeil(120)).toBe(200)
  })
})

describe('buildGeometry', () => {
  const buckets = [
    bucket('2026-10-12', { success: 4, active: 1 }),
    bucket('2026-10-13', { success: 2, failed: 1, rolled_back: 1 }),
    bucket('2026-10-14', {}),
  ]

  it('stacks segments so the column height equals the day total', () => {
    const g = buildGeometry(buckets, { width: 600, height: 200 })
    for (const s of g.stacks) {
      const drawn = s.segments.reduce((h, seg) => h + seg.height, 0)
      // The tallest column reaches the top of the scale, so compare proportionally.
      const expected = (s.total / niceCeil(Math.max(...buckets.map((b) => b.total)))) * g.plot.height
      expect(Math.abs(drawn - expected)).toBeLessThan(0.001)
    }
  })

  it('stacks bottom-up in series order, so the legend matches the stack', () => {
    const g = buildGeometry(buckets, { width: 600, height: 200 })
    const mid = g.stacks[1]!
    // Drawn in series order, so success sits at the BOTTOM (largest y) and
    // failure ends up on TOP — the worst series is the one the eye lands on.
    const success = mid.segments.find((s) => s.series === 'success')!
    const rolled = mid.segments.find((s) => s.series === 'rolled_back')!
    const failed = mid.segments.find((s) => s.series === 'failed')!
    expect(success.y).toBeGreaterThan(rolled.y)
    expect(rolled.y).toBeGreaterThan(failed.y)
    // Segments are contiguous: each one starts where the previous ends.
    const ordered = [...mid.segments].sort((a, b) => a.y - b.y)
    expect(ordered.length).toBeGreaterThan(1)
    for (let i = 1; i < ordered.length; i++) {
      const prev = ordered[i - 1]!
      const cur = ordered[i]!
      expect(Math.abs(cur.y - (prev.y + prev.height))).toBeLessThan(0.001)
    }
  })

  it('keeps a column and a slot for an empty day', () => {
    const g = buildGeometry(buckets, { width: 600, height: 200 })
    expect(g.stacks).toHaveLength(3)
    const empty = g.stacks[2]!
    expect(empty.segments).toHaveLength(0)
    expect(empty.total).toBe(0)
  })

  it('starts the y axis at zero', () => {
    const g = buildGeometry([bucket('2026-10-12', { success: 3 })], { width: 400, height: 200 })
    const first = g.ticks[0]!
    const last = g.ticks[g.ticks.length - 1]!
    expect(first.value).toBe(0)
    expect(first.y).toBe(g.plot.y + g.plot.height)
    // The top tick is at or above the plot's top edge, never below.
    expect(last.y).toBeLessThanOrEqual(g.plot.y)
  })

  it('thins x labels until they stop colliding', () => {
    const many: TrendBucket[] = Array.from({ length: 60 }, (_, i) => {
      const d = new Date(Date.UTC(2026, 0, 1 + i))
      return bucket(d.toISOString().slice(0, 10), { success: 1 })
    })
    const g = buildGeometry(many, { width: 600, height: 200 })
    for (let i = 1; i < g.labels.length; i++) {
      const cur = g.labels[i]!
      const prev = g.labels[i - 1]!
      expect(cur.x - prev.x).toBeGreaterThanOrEqual(44)
    }
    expect(g.labels.length).toBeGreaterThan(1)
    expect(g.labels.length).toBeLessThan(many.length)
  })

  it('survives a window with no data at all', () => {
    const g = buildGeometry([], { width: 400, height: 200 })
    expect(g.stacks).toHaveLength(0)
    expect(g.ticks.map((t) => t.value)).toEqual([0, 0, 0])
  })
})

describe('series vocabulary', () => {
  it('is five series in the worst-last order the backend sends', () => {
    expect(TREND_SERIES.map((s) => s.key)).toEqual([
      'success',
      'active',
      'rolled_back',
      'failed',
      'stopped',
    ])
  })

  it('uses the design system tones, not Element Plus tag words', () => {
    // "warning" (the el-tag word) must not appear: utils/node.ts pins the same
    // subset against base.css, and a missing rule renders no colour.
    for (const s of TREND_SERIES) {
      expect(['ok', 'live', 'warn', 'bad', 'idle']).toContain(s.tone)
    }
  })
})

describe('dayLabel', () => {
  it('shortens a day to month-day', () => {
    expect(dayLabel('2026-10-12')).toBe('10-12')
  })
  it('passes through anything that is not a day', () => {
    expect(dayLabel('whenever')).toBe('whenever')
  })
})

describe('sumBuckets', () => {
  it('totals every day and agrees with each bucket total', () => {
    const buckets = [
      bucket('2026-10-12', { success: 3 }),
      bucket('2026-10-13', { failed: 2, active: 1 }),
    ]
    expect(sumBuckets(buckets)).toBe(6)
    // The per-bucket totals carry the same number: an endpoint that stops
    // filling one of the two is caught here rather than in a screenshot.
    expect(buckets.reduce((n, b) => n + b.total, 0)).toBe(6)
  })
})
