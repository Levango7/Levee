import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the monitor's target map — the console's one surface whose
// cost grows without a ceiling (hosts × batches), and the one that was quietly
// broken by a layout bug as well as by volume.
//
// Four properties are pinned because each was learned the hard way:
//
//   1. the map is WINDOWED (only the visible slice is in the DOM). Measured
//      2026-10-12 on this repo's dev server: 2000 hosts × 12 batches went from
//      56,558 DOM nodes / a 61ms worst frame to 1,401 nodes / 12ms, and the
//      numbers are flat across 300/800/2000 hosts.
//   2. the verdict column keeps its width. When the batch columns were 72px
//      each, twelve of them squeezed "主机结论" to 34px, every chip wrapped to
//      four lines, and the row pitch was 64px — a layout bug that looked like a
//      data problem.
//   3. the row pitch is the MEASURED value. 21px is what the CSS suggests; 27
//      is what it renders at, and 21 would drift the scrollbar 1800px over 300
//      rows.
//   4. the spacer math keeps the scrollbar honest (pinned in the util's spec).

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
const view = locate('src/views/MonitorView.vue')
const util = locate('src/utils/targetMap.ts')

describe('target map: windowed, and the columns that were squeezed stay wide', () => {
  it('renders only the visible slice of rows', () => {
    expect(view, 'the list renders through the window, not the full array').toContain('v-for="r in visibleRows"')
    expect(view, 'a scroll handler feeds the window').toContain('onMapScroll')
    expect(view, 'the viewport is measured').toContain('measureMapViewport')
    // The spacers are what make the scrollbar describe the real row count; a
    // map without them would scroll 34 rows' worth and stop.
    expect((view.match(/map__spacer/g) || []).length).toBeGreaterThanOrEqual(2)
    expect(view, 'the windowed row count is shown, not hidden').toContain('显示 {{ visibleRows.length }} 行')
  })

  it('keeps the verdict and host columns wide enough never to wrap', () => {
    // The measured regression: twelve 72px batch columns left the verdict
    // column 34px, so every chip wrapped and rows were 64px tall.
    expect(view).toContain('white-space: nowrap')
    expect(view, 'the host column has a real width').toMatch(/\.map__host\s*\{[^}]*width:\s*132px/)
    expect(view, 'the verdict column has a real width').toMatch(/\.map__worst\s*\{[^}]*width:\s*92px/)
    expect(view, 'a batch column is narrow: it holds one dot').toMatch(/\.map__batch\s*\{[^}]*width:\s*46px/)
    // The header must not reintroduce the wrap with "批次 #12" in a 46px column.
    expect(view, 'the batch header is the number only').toContain('#{{ n }}</th>')
    expect(view, 'not the long form').not.toContain('批次 #{{ n }}</th>')
  })

  it('scrolls both axes inside its own box', () => {
    expect(view, 'the map owns its scroll container').toContain('class="map-scroll"')
    expect(view, 'and the header stays put while rows scroll').toContain('position: sticky')
    expect(view, 'vertical extent is capped').toMatch(/\.map-scroll\s*\{[^}]*max-height/)
  })

  it('pins the row pitch to the measured value', () => {
    // The CSS arithmetic says 21px (12px dot + 8px padding + 1px border); the
    // rendered pitch is 27px. Using the arithmetic value drifts the scrollbar
    // 6px per row — 1800px over 300 rows — and the window stops lining up.
    const m = /MAP_ROW_PITCH = (\d+)/.exec(util)
    expect(m, 'MAP_ROW_PITCH must be declared with a literal').not.toBeNull()
    expect(Number(m![1])).toBe(27)
  })

  it('does not window the small maps', () => {
    // A 20-host map renders every row: windowing it would add scroll geometry
    // for no gain and break "every host has a row" at a glance. This is the
    // util's own contract, pinned here so the view cannot bypass it.
    expect(util).toContain('windowed = count < total')
  })
})
