import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the template-instantiation wizard (this repo has no
// @vue/test-utils, so template shape is asserted against the source; the
// wizard's RULES live in utils/templateForm.ts and are unit-tested next door).
//
// What the wizard replaced: one dialog with a submit button that was reachable
// with a required parameter blank (the server refused it, after a round trip)
// and with the environment typed into a free-text box — while the
// authorization layer judges a change in the environment it declares, and the
// value is permanent.
//
// The four things this file keeps true:
//   1. there is no path from "parameters" to "created" that skips the review
//      step, and the review step is the only place the create button exists;
//   2. advancing is gated on the validation the unit tests cover;
//   3. the review step shows the values (parameters, environment, mode), not
//      just a summary word;
//   4. environment is a suggestion-list select, and the created change offers
//      a next action instead of a toast that disappears.
//
// `import.meta.url` is not a file: URL under vite's transform, so the file is
// located from the working directory, walking up (reads the same from web/ and
// from the repository root); not finding it is a hard failure.
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

const view = locate('src/views/TemplatesView.vue')

describe('template-instantiation wizard', () => {
  it('gates the step advance on validation', () => {
    const next = view.indexOf('function nextStep()')
    expect(next, 'the wizard must advance through nextStep()').toBeGreaterThan(-1)
    const body = view.slice(next, view.indexOf('function prevStep()'))
    expect(body, 'advancing must consult the validation result').toContain('issues.value.length > 0')
    expect(body, 'a refused advance must say why').toContain('ElMessage.warning')
  })

  it('exposes the create button only on the review step', () => {
    // One occurrence of the submit call in the template, behind the step check.
    const calls = view.split('@click="submitInstantiate"').length - 1
    expect(calls, 'submitInstantiate must be wired exactly once').toBe(1)
    const submit = view.indexOf('@click="submitInstantiate"')
    const gate = view.lastIndexOf('instantiate.step === 1', submit)
    expect(gate, 'the create button must be gated on the review step').toBeGreaterThan(-1)
  })

  it('shows the values on the review step, not just a summary', () => {
    const review = view.indexOf('instantiate.step === 1')
    const created = view.indexOf('instantiate.step === 2')
    expect(created).toBeGreaterThan(review)
    const block = view.slice(review, view.indexOf('</el-descriptions>', review))
    for (const shown of ['instantiate.form.label', 'paramRows', 'instantiate.form.environment', 'instantiate.form.priority']) {
      expect(block, `the review step must show ${shown}`).toContain(shown)
    }
    // and the mode must be spelled out, because "仅计划" alone does not say
    // that a planned change is a real row that skips approval-and-execute
    expect(view.slice(review, created)).toContain('创建方式')
  })

  it('offers environment suggestions instead of a free-text box', () => {
    const env = view.indexOf('v-model="instantiate.form.environment"')
    expect(env).toBeGreaterThan(-1)
    const block = view.slice(Math.max(0, env - 400), env + 400)
    expect(block, 'environment must be a select the caller can also type into').toContain('allow-create')
    expect(block, 'the suggestions come from the inventory').toContain('envOptions')
    // The suggestion list is a hint, not a whitelist: that is what allow-create
    // is for, and the gate that judges the value is the server's.
    expect(view).not.toContain('environment is required')
  })

  it('gives the created change a next action', () => {
    const done = view.indexOf('instantiate.step === 2')
    expect(done).toBeGreaterThan(-1)
    const block = view.slice(done)
    expect(block, 'the created change must be reachable').toContain('instantiate.createdId')
    expect(block, 'the wizard must offer the next step, not only a toast').toContain('查看监控')
  })

  it('resets on open so a second instantiation cannot inherit the first', () => {
    const open = view.indexOf('function openInstantiate')
    const body = view.slice(open, view.indexOf('async function submitInstantiate'))
    for (const reset of ['instantiate.step = 0', "instantiate.createdId = ''", 'blankForm(row)']) {
      expect(body, `openInstantiate must reset ${reset}`).toContain(reset)
    }
  })
})
