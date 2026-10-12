import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the console's first chart. The chart is hand-rolled SVG on
// purpose (bundle size, design-token fidelity), so the things that make it
// hand-rolled rather than accidental need pinning:
//
//   1. no charting library sneaks in (that is the decision, not a preference);
//   2. the series tones are the design system's own, and the token names it
//      uses must actually exist in styles/tokens.css — a tone that names a
//      missing token renders no colour at all, which is the failure mode
//      utils/node.ts guards on the dot side;
//   3. the geometry comes from the tested pure function, not from arithmetic
//      inlined in the template;
//   4. the panel that hosts it answers failures in place (StateBlock) rather
//      than swallowing them.

function locatePath(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (existsSync(p)) return p
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

const locate = (rel: string): string => readFileSync(locatePath(rel), 'utf8')
const chart = locate('src/components/TrendChart.vue')
const view = locate('src/views/ChangesView.vue')
const tokens = locate('src/styles/tokens.css')
const pkg = JSON.parse(locate('package.json'))

describe('trend chart: hand-rolled, on the design tokens, geometry tested', () => {
  it('does not pull in a charting library', () => {
    // The decision is documented in the component: a charting library is ~1MB
    // before the first bar, and its palette would be the only thing on the page
    // that does not match the design system. A dependency here reverses that
    // decision silently.
    const deps = { ...pkg.dependencies, ...pkg.devDependencies }
    for (const banned of ['echarts', 'chart.js', 'd3', '@antv/g2', 'vue-chartjs', 'recharts']) {
      expect(Object.keys(deps)).not.toContain(banned)
    }
    expect(chart, 'the chart draws its own SVG').toContain('<svg')
    expect(chart).not.toContain('import ECharts')
  })

  it('maps every series tone to a token the stylesheet defines', () => {
    // A tone that maps to a missing --lv-* token renders no colour: the bar is
    // invisible rather than wrong. The mapping lives in the component (tone ->
    // var(--lv-*)), so the check reads THAT rather than joining names — "live"
    // resolves to --lv-accent and "idle" to --lv-neutral-tone, neither of which
    // shares its own name.
    const seriesSrc = locate('src/utils/trend.ts')
    const tones = [...seriesSrc.matchAll(/tone: '(\w+)'/g)].map((m) => m[1])
    expect(tones.length).toBe(5)

    const vars = [...chart.matchAll(/var\((--lv-[\w-]+)\)/g)].map((m) => m[1])
    expect(vars.length).toBeGreaterThanOrEqual(5)
    for (const token of vars) {
      expect(tokens, `${token} must be defined in tokens.css`).toContain(token + ':')
    }
    // Every tone the series declare must appear in the component's map: a series
    // whose tone is missing falls back to idle and reads as "stopped".
    for (const tone of tones) {
      expect(chart, `tone ${tone} must be mapped`).toContain(tone)
    }
  })

  it('takes its geometry from the tested pure function', () => {
    expect(chart, 'geometry comes from utils/trend.ts').toContain(
      "import { TREND_SERIES, buildGeometry, dayLabel } from '@/utils/trend'",
    )
    expect(chart, 'no arithmetic duplicated in the template').toContain('buildGeometry(')
    // The component holds no data of its own: the parent owns the fetch and the
    // three non-ok states.
    expect(view, 'the view owns the fetch').toContain('trendApi.changeTrend')
    expect(view, 'the view owns the failure state').toContain('trendError')
    expect(view, 'failures answer in place, not as a toast').toContain('@retry="loadTrend"')
  })

  it('sends the operator timezone, not the server date', () => {
    // Day boundaries belong to the operator: without tz_offset the server
    // buckets on UTC and a UTC+8 operator's night work lands on the wrong day.
    expect(view, 'the request carries the browser offset').toContain('new Date().getTimezoneOffset()')
    expect(locate('src/api/index.ts'), 'and the api layer forwards it').toContain('tz_offset')
  })

  it('tells the operator when the window was capped', () => {
    // A truncated trend's right edge is the cap, not the data — a chart that
    // draws it silently is a chart that lies about volume.
    expect(view).toContain('trend.truncated')
    expect(view).toContain('上限')
  })
})

describe('trend chart: the pure geometry has its own spec', () => {
  it('is covered by unit tests, not only by a screenshot', () => {
    const dir = dirname(locatePath('src/utils/trend.ts'))
    const specs = readdirSync(dir).filter((f) => f.endsWith('.spec.ts'))
    expect(specs).toContain('trend.spec.ts')
    const spec = locate('src/utils/trend.spec.ts')
    // The property that would silently corrupt every chart: a column whose
    // segments do not sum to its total.
    expect(spec, 'the stacked sum is pinned').toContain('stacks segments so the column height equals the day total')
  })
})
