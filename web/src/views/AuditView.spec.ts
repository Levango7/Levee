import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the audit view's evidence side. The roadmap's row asks for
// "时间线 + hash 链校验按钮 + 证据片段卡", and two of the three were missing:
// the table proved WHERE a row sits in the chain but never showed what the
// change actually did (input/output), and the timeline the row asks for lives
// in the shared component the detail page uses.
//
// `import.meta.url` is not a file: URL under vite's transform, so files are
// located from the working directory, walking up.
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

const view = locate('src/views/AuditView.vue')

describe('audit view: evidence, timeline, verification', () => {
  it('offers an evidence fragment per row', () => {
    expect(view, 'the evidence lives in an expandable row').toContain('type="expand"')
    for (const field of ['row.input', 'row.output', 'row.detail']) {
      expect(view, `the evidence card must show ${field}`).toContain(field)
    }
    // An absent payload is stated, not left blank: "no output" is a fact about
    // the action, and an empty box reads like a loading failure.
    expect(view).toContain('该动作没有输入')
    expect(view).toContain('该动作没有输出')
  })

  it('shows the chain position inside the evidence card too', () => {
    const card = view.slice(view.indexOf('class="evidence"'), view.indexOf('</el-table-column>'))
    expect(card, 'the card carries the entry own links of the chain').toContain('row.prevHash')
    expect(card).toContain('row.currHash')
  })

  it('renders the timeline through the shared component, over the same page', () => {
    expect(view).toContain("import TraceTimeline from '@/components/TraceTimeline.vue'")
    expect(view).toContain('<TraceTimeline :entries="entries"')
    // One data set, two presentations: the timeline must not fetch its own page
    // or the two views would disagree about what "current" means.
    expect(view).toContain("v-if=\"viewMode === 'timeline'\"")
    expect(view).toContain('v-else')
    expect(view, 'no local timeline rendering').not.toContain('el-timeline-item')
  })

  it('keeps the chain verification entry point', () => {
    expect(view).toContain('哈希链校验')
    expect(view).toContain('auditApi.verifyHashChain')
  })

  it('keeps one timeline renderer for trace entries', () => {
    // Anti-twin guard: any view that renders the Element timeline markup must
    // go through the shared component — except the ONE documented exception
    // below. A new view that grows its own timeline is flagged here rather than
    // drifting quietly, and the exception is a name, not a category: it cannot
    // absorb new offenders.
    const viewsDir = dirname(locatePath('src/views/AuditView.vue'))
    const files = readdirSync(viewsDir).filter((f) => f.endsWith('.vue'))
    const ownTimeline: string[] = []
    for (const f of files) {
      const src = readFileSync(join(viewsDir, f), 'utf8')
      if (src.includes('el-timeline-item') && !src.includes('<TraceTimeline')) ownTimeline.push(f)
    }
    expect(files.length).toBeGreaterThan(5)
    // MobileApprovalView renders the APPROVAL HISTORY: a decision timeline
    // coloured by approve/reject, with the actor and comment but no target host
    // or duration — a different emphasis from the trace timeline, on a page with
    // its own mobile design. It is not a trace renderer, so it stays. The day it
    // starts rendering trace entries, it moves to the shared component and this
    // list empties.
    expect(ownTimeline.sort()).toEqual(['MobileApprovalView.vue'])
  })
})
