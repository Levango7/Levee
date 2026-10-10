import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the partial-rollback remediation entry, not a render test
// (this repo has no @vue/test-utils). The roadmap records the code layer as
// ready — POST /changes/{id}/rollback is idempotent — while the UI had the two
// verdicts only as labels: an operator staring at 部分回滚 / 回滚未完成 had no
// button that finishes the compensation, and 重试 (which re-drives the FORWARD
// change) was the only affordance on the row.
//
// What has to stay true, asserted against the source of the view and the helper:
//   - the button is gated on isRemediableStatus (the owned vocabulary), so a new
//     status cannot drift into it and the two verdicts cannot drift out;
//   - the dialog shows evidence BEFORE anything moves (a dry_run call is made
//     first) and the commit is the same endpoint without dry_run;
//   - the dialog body is built as VNodes, not an HTML string — the host names
//     and the server's message come from the API and must not become markup.
//
// `import.meta.url` is not a file: URL under vite's transform, so the files are
// located from the working directory, walking up (reads the same from web/ and
// from the repository root); not finding them is a hard failure.
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

const view = locate('src/views/ChangesView.vue')

describe('partial-rollback remediation entry', () => {
  it('gates the 补救 button on the owned status set', () => {
    expect(view).toContain("import { formatTimestamp, isRemediableStatus, isRetryableStatus } from '@/utils/format'")
    // The button must exist *and* be gated by the helper — a bare "补救" that
    // renders on every row would offer an action that cannot do anything on a
    // completed change.
    const gate = view.indexOf('v-if="isRemediableStatus(row.status)"')
    expect(gate, 'the action must be gated on isRemediableStatus(row.status)').toBeGreaterThan(-1)
    const call = view.indexOf('@click.stop="remediate(row)"')
    expect(call, 'the row must expose the remediation action').toBeGreaterThan(-1)
    // The gate and the handler must be on the same element: within the opening
    // tag that the v-if starts, and before the next sibling button.
    const window = view.slice(gate, call)
    expect(window, 'the gate must open an element whose handler is the remediation call').not.toContain('<el-button')
  })

  it('keeps 补救 and 重试 as two separate affordances', () => {
    // They are different intents on these two states (finish the rollback vs
    // re-drive the change) and the earlier UI had only the latter.
    expect(view).toContain('@click.stop="retryChange(row)"')
    expect(view).toContain('v-if="isRetryableStatus(row.status)"')
  })

  it('delegates the flow to the shared composable instead of carrying a copy', () => {
    // The order that matters (dry_run preview -> evidence -> commit) lives in
    // composables/useRollbackRemediation.ts, shared with the change detail
    // page; its own spec asserts that order. What this view must not do is
    // grow a second copy: the list and the detail page would then drift on the
    // pairing, which is exactly the failure this area is about.
    expect(view).toContain("import { remediateRollback } from '@/composables/useRollbackRemediation'")
    expect(view).toContain('await remediateRollback(row)')
    expect(view).not.toContain('changesApi.rollback(')
    expect(view).not.toContain('dangerouslyUseHTMLString: true')
  })
})
