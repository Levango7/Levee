import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the column-personalisation row. Three properties matter
// and each was a decision rather than an accident:
//
//   1. the rules live in ONE owner (utils/tablePrefs) — a view that re-implements
//      "unknown names are dropped" or "the last column cannot be hidden" will get
//      one of them wrong;
//   2. the view only decides WHICH columns are optional, and always leaves the
//      identity columns visible;
//   3. drag-to-resize stays out (documented why in the util), because EP's table
//      has no native resize and min-width + overflow tooltips already answer the
//      horizontal pressure.

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
const util = locate('src/utils/tablePrefs.ts')
const view = locate('src/views/ChangesView.vue')

describe('column preferences: one owner, and the view keeps the identity columns', () => {
  it('routes through the shared utility', () => {
    expect(view, 'the view imports the owner, not localStorage directly').toContain(
      "from '@/utils/tablePrefs'",
    )
    expect(view, 'and does not hand-roll storage access').not.toContain('localStorage')
  })

  it('leaves the identity columns always visible', () => {
    // 变更 (name), 状态 (status) and 操作 (actions) are what a row IS; hiding
    // them leaves a table of nothing recognizable.
    expect(view).toContain("const OPTIONAL_COLUMNS = ['scope', 'template', 'created'] as const")
    expect(view, 'the name column has no v-if').not.toMatch(/<el-table-column v-if[^>]*label="变更"/)
    expect(view, 'the status column has no v-if').not.toMatch(/<el-table-column v-if[^>]*label="状态"/)
    expect(view, 'the actions column has no v-if').not.toMatch(/<el-table-column v-if[^>]*label="操作"/)
  })

  it('tells the operator when a hide is refused', () => {
    // A click that does nothing is indistinguishable from a broken page.
    expect(view, 'the refusal is announced').toContain('至少保留一列')
    expect(util, 'and the util reports it rather than throwing').toContain('refused: boolean')
  })

  it('keeps drag-to-resize out, on purpose', () => {
    // min-width columns + overflow tooltips answer the horizontal pressure;
    // native resize does not exist on EP's table and hand-rolling it means
    // pointer capture, keyboard equivalents and touch handling.
    expect(view, 'no pointer-drag handlers on columns').not.toContain('mousedown')
    expect(util, 'no width math').not.toMatch(/width\s*[:=]/)
  })

  it('is defensive about storage', () => {
    // Private-mode Safari throws on localStorage; a preference is never worth an
    // exception at boot (useTheme guards the same way).
    expect(util).toContain('try {')
    expect(util).toContain('} catch {')
    expect(util, 'unknown names are dropped').toContain('function sanitize')
  })
})

describe('column preferences: covered by unit tests, not only by structure', () => {
  it('has a spec for the owner', () => {
    const dir = dirname(locatePath('src/utils/tablePrefs.ts'))
    expect(readdirSync(dir)).toContain('tablePrefs.spec.ts')
    const spec = locate('src/utils/tablePrefs.spec.ts')
    expect(spec, 'the last-column rule is pinned').toContain('refuses to hide the last visible column')
    expect(spec, 'stale names are pinned').toContain('drops stale column names from an older build')
    expect(spec, 'the throwing-storage case is pinned').toContain('storage that throws')
  })
})
