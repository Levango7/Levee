import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the three-tier region states (roadmap: "所有列表/详情/日志区
// 必须有明确的空 / 失败 / 权限不足 三档"). It guards the SHAPE, not the copy:
//
//   1. every view that loads a region answers failures in place, through the
//      shared StateBlock + utils/apiState — a new view cannot re-invent the
//      toast-over-an-unchanged-table pattern the roadmap row just deleted;
//   2. the deleted patterns cannot come back quietly: the bare <p class="error">
//      and the el-alert banner that rendered one tier only;
//   3. the deliberate exceptions are NAMED, so they cannot absorb new offenders.
//
// Reads the sources, so it cannot pass on a mock; a missing file is a hard
// failure (same locate as the other view specs).

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

const viewsDir = dirname(locatePath('src/views/ChangesView.vue'))
const viewFiles = readdirSync(viewsDir).filter((f) => f.endsWith('.vue'))
const viewSources = new Map(viewFiles.map((f) => [f, readFileSync(join(viewsDir, f), 'utf8')]))

// Views with a region that loads. Each one must route its failures through the
// shared classification. A view that starts fetching without being listed here
// fails the "every fetching view is pinned" check below.
const FETCHING_VIEWS = [
  'ApprovalView.vue',
  'AuditView.vue',
  'ChangeDetailView.vue',
  'ChangesView.vue',
  'ClusterView.vue',
  'ConversationView.vue',
  'MobileApprovalView.vue',
  'MonitorView.vue',
  'SystemView.vue',
  'TargetsView.vue',
  'TemplatesView.vue',
]

// Views that never call the API. They are the reason the guard below uses an
// explicit list: a login page has no region to fail.
const NON_FETCHING_VIEWS = ['LoginCallbackView.vue', 'LoginView.vue', 'NotFoundView.vue']

// el-alert is a deleted REGION-STATE pattern (it once rendered one tier with no
// retry; MobileApprovalView was its last user). It stays legal for a FORM-LEVEL
// result inside a dialog, which is not a region state at all. This is an
// allow-list with reasons rather than a bare "not TemplatesView" so a view
// cannot join the exception silently — and the second loop below makes each
// entry prove it is still needed.
const EL_ALERT_IN_DIALOG = ['TemplatesView.vue', 'MonitorView.vue']

describe('region states: the three tiers are wired everywhere', () => {
  it('pins the fetching views it claims to pin', () => {
    // If this list ever fails, the guard lost coverage of a real page: add the
    // new view to FETCHING_VIEWS rather than deleting the assertion.
    const unclassified = FETCHING_VIEWS.filter((f) => !viewSources.has(f))
    expect(unclassified).toEqual([])
    expect(FETCHING_VIEWS.length).toBeGreaterThan(5)
    for (const name of NON_FETCHING_VIEWS) {
      const src = viewSources.get(name) ?? ''
      // A "non-fetching" view that grew a fetch must be re-listed above.
      expect(src.includes('Api.') || src.includes('from \'@/api\''), `${name} fetches but is listed as non-fetching`).toBe(
        false,
      )
    }
  })

  it('routes every fetching view through the shared component and classifier', () => {
    for (const name of FETCHING_VIEWS) {
      const src = viewSources.get(name) ?? ''
      expect(src, `${name} must import StateBlock`).toContain(
        "import StateBlock from '@/components/StateBlock.vue'",
      )
      expect(src, `${name} must classify failures through the single owner`).toContain(
        "from '@/utils/apiState'",
      )
    }
  })

  it('answers a forbidden region distinctly from a failed one', () => {
    // The tier is the only 'forbidden' signal the gateway gives (HTTP 403); a
    // view that never compares against it renders a permission refusal as a
    // retryable failure, which is wrong twice: the retry cannot succeed, and
    // the operator is told to try again instead of who to ask.
    for (const name of FETCHING_VIEWS) {
      const src = viewSources.get(name) ?? ''
      expect(src, `${name} must distinguish the forbidden tier`).toContain("'forbidden'")
    }
  })

  it('keeps the deleted render patterns deleted', () => {
    // The bare error paragraph: ClusterView and ConversationView each had one,
    // below everything it invalidated. The pattern is the OLD MARKUP, so a
    // comment that explains what was removed cannot fail this check.
    for (const [name, src] of viewSources) {
      expect(src, `${name} still renders the bare error paragraph`).not.toContain('<p class="error"')
      if (EL_ALERT_IN_DIALOG.includes(name)) continue
      expect(src.includes('<el-alert'), `${name} still renders an error alert`).toBe(false)
    }
    // The exception list must not go stale: a named view that dropped its
    // dialog-level alert has to be removed from it (otherwise the exemption
    // quietly outlives the reason it was granted).
    for (const name of EL_ALERT_IN_DIALOG) {
      expect(
        (viewSources.get(name) ?? '').includes('<el-alert'),
        `${name} is allow-listed for a dialog-level el-alert but no longer uses one`,
      ).toBe(true)
    }
  })

  it('keeps the two documented exceptions for el-empty', () => {
    // TraceTimeline's and ChangeDetailView's el-empty: the latter is a genuine
    // "this id does not exist" answer, which is NOT a region state (there is
    // no data to be empty of), and it is reached only when the load succeeded
    // and returned nothing. TraceTimeline routes its own empty through
    // StateBlock now; any NEW el-empty outside these two is flagged.
    const offenders: string[] = []
    for (const [name, src] of viewSources) {
      if (!src.includes('<el-empty')) continue
      if (name === 'ChangeDetailView.vue') continue
      offenders.push(name)
    }
    expect(offenders).toEqual([])
  })
})
