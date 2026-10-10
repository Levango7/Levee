import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the change detail page. `/changes/:id` used to route back
// to the list view, so every "详情" click landed on the table again; the page
// exists now and the properties worth keeping are the ones that made it worth
// building:
//
//   - the timeline is shown WITH its chain verdict (a story without "is this
//     record intact" is exactly what the audit trail exists to prevent),
//   - the batch panel uses the batch vocabulary's own helpers rather than
//     re-labelling states locally,
//   - every action is gated by the helper that mirrors the server's switch, so
//     the page cannot offer a button the RPC would refuse,
//   - remediation goes through the shared flow (no second copy of the
//     dry-run/commit pairing),
//   - the log panel says it is a tail, not the live stream.
function locate(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (existsSync(p)) return readFileSync(p, 'utf8')
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

const view = locate('src/views/ChangeDetailView.vue')
const router = locate('src/router/index.ts')

describe('change detail page', () => {
  it('is the view the detail route actually renders', () => {
    const route = router.slice(router.indexOf("path: '/changes/:id'"))
    const nextRoute = route.indexOf("path: '/login'")
    const block = route.slice(0, nextRoute > 0 ? nextRoute : undefined)
    expect(block, 'the detail route must render the detail view').toContain(
      "import('@/views/ChangeDetailView.vue')",
    )
    expect(block).not.toContain("import('@/views/ChangesView.vue')")
  })

  it('shows the timeline with the chain verdict, from one verified call', () => {
    expect(view, 'the trace must be fetched with verification on').toContain("trace(changeId.value, { verify: true })")
    expect(view, 'the verdict must be rendered, not just fetched').toContain('hashChainValid')
    expect(view, 'the verdict must be labelled for a reader').toContain('哈希链')
    // The rendering itself is the shared component (components/TraceTimeline),
    // which the audit view also uses; this view must not grow a second copy.
    expect(view, 'the entries feed the shared timeline').toContain('<TraceTimeline :entries="timeline"')
    expect(view, 'no local timeline rendering').not.toContain('el-timeline-item')
  })

  it('renders batches through the batch vocabulary, not a local map', () => {
    for (const helper of ['batchLabel', 'batchBarStatus', 'batchProgress']) {
      expect(view, `the batch panel must use ${helper}`).toContain(helper)
    }
    // Labels for the batch states live in utils/batch.ts. A local map would
    // need the server's state tokens as keys, which is what this refuses —
    // matched as map keys, not as words: the panel legitimately prints counts
    // like "成功 3 · 失败 1", and those are numbers, not state labels.
    expect(view, 'no local batch-state label map').not.toMatch(/completed\s*:\s*['"]/)
    expect(view, 'no local batch-state label map').not.toMatch(/rolled_back\s*:\s*['"]/)
  })

  it('gates every action with the helper that mirrors the server switch', () => {
    const gates = [
      ['isRemediableStatus', '补救'],
      ['isRetryableStatus', '重试'],
      ['isPausableStatus', '暂停'],
      ['isResumableStatus', '恢复'],
      ['isCancellableStatus', '取消'],
      ['isArchivableStatus', '归档'],
    ]
    for (const [helper, label] of gates) {
      expect(gates.length, `${label} must have a gate`).toBeGreaterThan(0)
      expect(view, `${label} must be gated by ${helper}`).toContain(`v-if="${helper}(change.status)"`)
    }
    // …and all of them come from the shared module rather than being re-derived.
    const imports = view.slice(view.indexOf("from '@/utils/format'") - 500, view.indexOf("from '@/utils/format'"))
    for (const [helper] of gates) {
      expect(imports, `${helper} must be imported from utils/format`).toContain(helper)
    }
  })

  it('delegates remediation to the shared flow', () => {
    expect(view).toContain("import { remediateRollback } from '@/composables/useRollbackRemediation'")
    expect(view).toContain('await remediateRollback(change.value)')
    expect(view, 'the page must not carry its own rollback sequence').not.toContain('changesApi.rollback(')
  })

  it('labels the log panel as a tail, not the live stream', () => {
    expect(view).toContain('尾部')
    expect(view, 'the live stream lives on the monitor page').toContain('实时监控')
    expect(view, 'the truncation flag must reach the reader').toContain('logsTruncated')
  })

  it('reads the timeline newest-first', () => {
    // An operator arriving at a failed change wants the verdict and the last
    // thing that happened; an ascending list buries both under the first steps.
    const sorter = view.indexOf('const timeline = computed')
    expect(sorter, 'the timeline must be ordered explicitly').toBeGreaterThan(-1)
    expect(view.slice(sorter, view.indexOf('const batchRows', sorter))).toContain('b.timestamp - a.timestamp')
  })
})
