// trend.ts — the pure math between the trend endpoint's buckets and the SVG
// the chart draws. It is separate from the component for the same reason
// targetMap.ts is: the geometry is where the bugs live (a stacked bar whose
// segments do not sum to its total, an axis whose labels skip a day), and
// geometry is testable without a DOM.

/** One outcome series, in the order the endpoint's `series` field gives. */
export interface TrendSeries {
  key: string
  label: string
  /** CSS colour token name (without the var() wrapper), e.g. "ok". */
  tone: string
}

/**
 * The five series and their tones. The ORDER is the legend order the backend
 * sends (worst last): success at the bottom of a stack keeps the readable end
 * at the top and puts failure where the eye lands first.
 *
 * The tones are the design system's own (styles/tokens.css), NOT Element
 * Plus's: "warning" reads almost the same as the real class "warn" and a
 * missing rule renders no colour at all — the guard in utils/node.ts pins the
 * same subset against the stylesheet.
 */
export const TREND_SERIES: TrendSeries[] = [
  { key: 'success', label: '成功', tone: 'ok' },
  { key: 'active', label: '进行中', tone: 'live' },
  { key: 'rolled_back', label: '已回滚', tone: 'warn' },
  { key: 'failed', label: '失败', tone: 'bad' },
  { key: 'stopped', label: '已中止', tone: 'idle' },
]

/** One day's row as the endpoint returns it. */
export interface TrendBucket {
  day: string
  total: number
  outcomes: Record<string, number>
}

export interface StackSegment {
  series: string
  value: number
  /** Pixels from the top of the plot area; height is the segment's height. */
  y: number
  height: number
}

export interface DayStack {
  day: string
  total: number
  segments: StackSegment[]
  /** Plot x of this day's column. */
  x: number
  width: number
}

export interface ChartGeometry {
  width: number
  height: number
  /** The plot area inside the margins. */
  plot: { x: number; y: number; width: number; height: number }
  stacks: DayStack[]
  /** Y axis ticks: value + pixel y, ascending. */
  ticks: Array<{ value: number; y: number }>
  /** X axis labels: day + pixel x, thinned to fit. */
  labels: Array<{ day: string; x: number }>
}

/**
 * buildGeometry turns buckets into the bars of a stacked column chart.
 *
 * Two deliberate choices:
 *
 *   - The y scale starts at ZERO. A truncated axis makes a small change look
 *     like a cliff; on a governance chart that is the difference between "one
 *     change failed" and "everything is on fire".
 *   - Days with no runs still get a column (of zero height) and a slot in the
 *     axis, so a gap reads as a gap instead of the chart silently closing the
 *     ranks.
 */
export function buildGeometry(
  buckets: TrendBucket[],
  opts: { width: number; height: number; margin?: Partial<{ top: number; right: number; bottom: number; left: number }> },
): ChartGeometry {
  const margin = { top: 12, right: 8, bottom: 22, left: 34, ...(opts.margin || {}) }
  const width = Math.max(opts.width, 120)
  const height = Math.max(opts.height, 80)
  const plot = {
    x: margin.left,
    y: margin.top,
    width: Math.max(width - margin.left - margin.right, 1),
    height: Math.max(height - margin.top - margin.bottom, 1),
  }

  const max = buckets.reduce((m, b) => Math.max(m, b.total), 0)
  // Round the top of the scale to a "nice" number so the ticks read as
  // 0 / 5 / 10 rather than 0 / 3 / 6 / 9 / 12.
  const top = niceCeil(max)
  const scale = (v: number): number => (top === 0 ? 0 : (v / top) * plot.height)

  const n = buckets.length
  const slot = plot.width / Math.max(n, 1)
  // The column is 62% of its slot, capped so a 7-day chart does not draw
  // cathedral-wide bars and a 90-day chart still has slivers to read.
  const colWidth = Math.min(slot * 0.62, 44)

  const stacks: DayStack[] = buckets.map((b, i) => {
    const x = plot.x + i * slot + (slot - colWidth) / 2
    let cursor = plot.y + plot.height
    const segments: StackSegment[] = []
    // Draw in series order so the first series sits at the BOTTOM of the
    // column and the legend matches the stack.
    for (const s of TREND_SERIES) {
      const value = b.outcomes[s.key] || 0
      if (value <= 0) continue
      const h = scale(value)
      cursor -= h
      segments.push({ series: s.key, value, y: cursor, height: h })
    }
    return { day: b.day, total: b.total, segments, x, width: colWidth }
  })

  const ticks = [0, top / 2, top].map((value) => ({
    value: Math.round(value),
    y: plot.y + plot.height - scale(value),
  }))

  // One label per slot until they would collide; below that every k-th.
  const minGap = 44
  const every = Math.max(1, Math.ceil(minGap / Math.max(slot, 1)))
  const labels = buckets
    .map((b, i) => ({ day: b.day, x: plot.x + i * slot + slot / 2 }))
    .filter((_, i) => i % every === 0)

  return { width, height, plot, stacks, ticks, labels }
}

/** niceCeil rounds up to 1/2/5 × a power of ten. */
export function niceCeil(v: number): number {
  if (v <= 0) return 0
  if (v <= 5) return 5
  const exp = Math.floor(Math.log10(v))
  const pow = Math.pow(10, exp)
  const frac = v / pow
  const nice = frac <= 1 ? 1 : frac <= 2 ? 2 : frac <= 5 ? 5 : 10
  return nice * pow
}

/** Short day label: "2026-10-12" -> "10-12". */
export function dayLabel(day: string): string {
  const m = /^\d{4}-(\d{2})-(\d{2})$/.exec(day)
  return m ? `${m[1]}-${m[2]}` : day
}

/**
 * sumBuckets totals every day, for the panel's "共 N 个变更" line. Deliberately
 * not read from `total` on each bucket: the guard test asserts the two agree,
 * which is what catches an endpoint that stops filling one of them.
 */
export function sumBuckets(buckets: TrendBucket[]): number {
  return buckets.reduce((n, b) => n + b.total, 0)
}
