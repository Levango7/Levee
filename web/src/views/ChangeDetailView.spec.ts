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

  it('asks the read for the stored plan and never generates one to show it', () => {
    // Two endpoints can produce a Plan object and only one of them belongs on a
    // detail page: GET /changes/:id?includePlan=true returns the artifact the run
    // already carries (the one plan_hash binds and the approval was voted
    // against), while POST /changes/:id/plan GENERATES a plan. A read page that
    // minted artifacts would change what it claims to merely display — and on a
    // page whose whole point is "this is what was approved", that is not cosmetic.
    expect(view, 'must fetch the change with includePlan').toContain(
      'changesApi.get(changeId.value, { includePlan: true })',
    )
    expect(view, 'must not call the generating endpoint').not.toContain('changesApi.plan(')
  })

  it('renders the plan from the artifact, and names the no-plan state as a draft', () => {
    expect(view, 'the panel must be titled').toContain('执行计划')
    expect(view, 'batch rows must come straight from the stored plan').toContain(
      'change.value?.plan?.batches ?? []',
    )
    expect(view, 'the target set must come straight from the stored plan').toContain(
      'change.value?.plan?.targetHosts ?? []',
    )
    expect(view, 'the impact summary must reach the reader').toContain('change.plan.impactSummary')
    expect(view, 'per-batch targets must reach the reader').toContain('{{ b.hosts.join')
    // protojson omits zero values: batch 0 has no `index` key in the JSON at all,
    // and a batch with no concurrency cap has no `maxConcurrency`. A template
    // reading either as a number renders a blank cell — the same silent-zero
    // family this page's contract guards exist for, so the absent case is what
    // the markup has to handle.
    expect(view, 'the batch ordinal must tolerate an omitted index').toContain('b.index ?? 0')
    expect(view, 'the concurrency cap must render 不限 rather than blank').toContain("b.maxConcurrency || '不限'")
    // "no plan" is a real state for a draft, not a failure — the server answers
    // with no plan rather than an error, and the page must say which of the two
    // the operator is looking at.
    expect(view, 'the draft case must be labelled, not left blank').toContain('还没有存量计划')
    // Three names the proto declares but the gateway never sends (zero writers in
    // Go, so protojson omits them). Rendering one would print a permanent blank
    // cell and read as "the plan says zero"; the types dropped them for the same
    // reason, and this stops them creeping back.
    for (const unreachable of ['estimatedDuration', 'potentialConflicts', 'waitAfter']) {
      expect(view, `the plan panel must not read ${unreachable}: the wire never sends it`).not.toContain(unreachable)
    }
  })

  it('shows the approval chain read-only, sourced from the shared helper', () => {
    // "Where is the sign-off" is a first-class part of reading a change: the
    // quorum it needs, the votes already cast, who still owes one, and whether
    // an approval went stale. The read route and the client method already
    // existed; what the page lacked was calling them. Approving itself lives in
    // the approval centre, so this panel is labelled read-only rather than
    // offering buttons that belong elsewhere.
    expect(view, 'must fetch the approval chain').toContain('changesApi.approvals(changeId.value)')
    expect(view, 'must title the panel 审批链').toContain('审批链')
    expect(view, 'must mark it read-only so it is not mistaken for the act page').toContain('只读')
    expect(view, 'quorum needed must reach the reader').toMatch(/需 \{\{ c\.minApprovers \}\} 票/)
    expect(view, 'votes cast must reach the reader').toMatch(/已投 \{\{ c\.decided \}\} 票/)
    expect(view, 'who still owes a vote must reach the reader').toContain('未投：')
    // A stale approval is void even though the run's own status still reads
    // approved — dropping it would tell the operator the change is cleared when
    // it is not.
    expect(view, 'a stale approval must be surfaced, not hidden').toContain('需重新批准')
  })

  it('derives the vote tally from approvalSummary, never a local copy', () => {
    // approvalSummary owns "how many have decided" and "who is still pending".
    // If this view re-tallied them it could show a different quorum picture than
    // the approval centre for the same chain — the exact drift the helper exists
    // to prevent, and the reason it is unit-tested pure.
    expect(view, 'must import the shared summary helper').toContain("import { approvalSummary } from '@/utils/approvalChain'")
    expect(view, 'rows must come from the helper').toContain('approvalSummary(approvals.value)')
    expect(view, 'no local decision count').not.toMatch(/\.decisions\.length/)
    expect(view, 'no local pending-set derivation').not.toMatch(/new Set\(/)
  })
})
