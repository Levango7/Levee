import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the shared remediation flow. It is shared by the change
// list and the change detail page, so the properties asserted here are the ones
// both depend on; the render-level behaviour was exercised against the repo's
// dev server when the flow was introduced (dry_run preview then commit, call
// order verified in the request log).
//
// The order is the point: a dry_run preview whose result is never shown, or a
// commit that runs without a preview, are both plausible edits to this file and
// both destroy the "show the evidence, then ask once" design.
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

const src = locate('src/composables/useRollbackRemediation.ts')

describe('useRollbackRemediation', () => {
  it('previews with dry_run before it commits, and commits without it', () => {
    const preview = src.indexOf('dryRun: true')
    expect(preview, 'the flow must preview with dry_run').toBeGreaterThan(-1)
    const commit = src.indexOf('changesApi.rollback(change.id, {})', preview)
    expect(commit, 'the commit must come after the preview').toBeGreaterThan(-1)
    const confirm = src.indexOf("'补救未完成的回滚'", preview)
    expect(confirm, 'the operator must be asked between the two calls').toBeGreaterThan(-1)
    expect(confirm).toBeLessThan(commit)
  })

  it('feeds the dialog from the preview own fields', () => {
    const preview = src.indexOf('preview.rolledBackHosts')
    expect(preview, 'the evidence is the preview result, not a re-query').toBeGreaterThan(-1)
    expect(src).toContain('preview.skippedHosts')
    expect(src).toContain('preview.message')
  })

  it('builds the dialog body as VNodes, not as an HTML string', () => {
    expect(src).toContain("import { h } from 'vue'")
    expect(src).toContain("h('div', [")
    // The property form is what would pass API-supplied host names and messages
    // straight into the DOM as markup. Matched as a property, not as a word:
    // the file's own comment explains why it is not used, and a mention is not
    // a usage.
    expect(src).not.toMatch(/dangerouslyUseHTMLString\s*:/)
  })

  it('is the only implementation: no caller carries its own rollback sequence', () => {
    for (const caller of ['src/views/ChangesView.vue', 'src/views/ChangeDetailView.vue']) {
      const text = locate(caller)
      expect(text, `${caller} must call the shared flow`).toContain('remediateRollback')
      expect(text, `${caller} must not call the rollback endpoint itself`).not.toContain(
        'changesApi.rollback(',
      )
    }
  })

  it('says what remediation is, and what it is not', () => {
    // The dialog has to distinguish finishing a rollback from re-driving the
    // forward change, because the two are one row apart in both views.
    expect(src).toContain('重试')
    expect(src).toContain('幂等')
  })
})
