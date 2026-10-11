import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the approval chain card. The roadmap's row asks the
// console to show the admission gate's intermediate state "clearly side by
// side", because a vote that cannot land -- quorum short, or the approver is
// the initiator, or the plan moved -- looks identical to "nobody has approved
// yet".
//
// What has to stay true: the card is fed by the new read path
// (changes/approvals), the quorum and the not-yet-voted set are shown (that is
// the difference between "no" and "nobody"), the initiator barring and the
// legacy-chain flag are stated rather than inferred, and a load failure is
// shown as a failure instead of an empty card.
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

const view = locate('src/views/ApprovalView.vue')

describe('approval centre: the chain card', () => {
  it('fetches the chain through the new read path, when the row expands', () => {
    expect(view).toContain('changesApi.approvals(row.id)')
    expect(view, 'the fetch must be lazy (an expand), not one request per row on load')
      .toContain('function loadChains')
    expect(view).toContain('@expand-change="onExpand"')
    expect(view, 'a row already loaded must not be fetched twice').toContain(
      'if (chains.value[row.id] || chainsLoading.value[row.id]) return',
    )
  })

  it('shows the quorum and who has not voted, not just the count', () => {
    expect(view).toContain('c.minApprovers')
    expect(view).toContain('c.decided')
    expect(view).toContain('c.pending')
    // The joined names are the answer to "why is this still pending".
    expect(view).toContain('c.pending.join')
  })

  it('states the two facts that used to be invisible', () => {
    expect(view, 'the initiator barring').toContain('c.excludeInitiator')
    expect(view, 'the initiator identity').toContain('c.initiator')
    expect(view, 'a legacy chain says so instead of rendering a blank hash').toContain('c.legacy')
  })

  it('surfaces a load failure instead of an empty card', () => {
    expect(view).toContain('chainsError')
    expect(view).toContain('chain__error')
    // An empty chain is a distinct state from a failed load.
    expect(view).toContain('暂无审批链记录')
  })

  it('keeps the derived facts in one shared helper', () => {
    expect(view).toContain("import { approvalSummary, type ApprovalChain } from '@/utils/approvalChain'")
    const helper = locate('src/utils/approvalChain.ts')
    expect(helper).toContain('pending: c.approvers.filter((a) => !done.has(a))')
    expect(helper).toContain("export type { ApprovalChain } from '@/api'")
  })
})
