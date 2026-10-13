import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for how a change's priority reaches the screen.
//
// The console used to print the wire spelling (`{{ change.priority }}`), so a
// Chinese page showed "urgent" where it should show 紧急 — the same class of defect
// this repo already closed for batch, node and status labels. utils/format.ts has
// carried PRIORITY_LABEL the whole time with exactly one consumer: its own unit
// test. No view ever read it.
//
// What is pinned is the SHAPE, not the copy:
//   1. the set of views that render a priority is declared here, in either shape,
//      so a fifth page cannot join quietly;
//   2. each declared page routes through priorityLabel, imported from its owner;
//   3. the deleted shapes cannot come back — a bare `{{ …priority… }}`
//      interpolation, and the instantiate wizard's four hand-copied <el-option>
//      lines (a third owner of the vocabulary, beside the union and the table);
//   4. the wizard's form type still carries the vocabulary rather than an open
//      string, which is what let it build a request the entrypoint now refuses.
//
// Sources are read from disk, so this cannot pass on a mock, and a missing file is
// a hard failure (same locate idiom as the other view specs).

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
const formatPath = locatePath('src/utils/format.ts')
const templateFormPath = locatePath('src/utils/templateForm.ts')

// Views that put a change's priority on screen, and the expression each renders.
// Naming the expression makes check 2 specific: the regression is this exact
// value reaching the DOM unlabelled, not any other use of the word "priority".
const PRIORITY_VIEWS: Record<string, string> = {
  'ApprovalView.vue': 'row.priority',
  'ChangeDetailView.vue': 'change.priority',
  'MonitorView.vue': 'change.priority',
  'TemplatesView.vue': 'instantiate.form.priority',
}

const IMPORTS_HELPER = /import\s*\{[^}]*\bpriorityLabel\b[^}]*\}\s*from\s*'@\/utils\/format'/
// A template interpolation that carries a priority value. The word boundary is the
// whole trick: `priorityLabel(` and `PRIORITY_LABEL[` must NOT match (an identifier
// continues into "Label"), while `row.priority` and `instantiate.form.priority` do.
// So a line matching this is "the page shows a priority", and the check below is
// that every such line shows it through the helper.
const PRIORITY_INTERPOLATION = /\{\{[^}]*\bpriority\b[^}]*\}\}/i

// Whole-line comments and template comments are stripped first: this file family
// documents the vocabulary in prose, and prose mentions the word.
function scriptCode(source: string): string {
  return source
    .split('\n')
    .filter((line) => !line.trim().startsWith('//') && !line.trim().startsWith('<!--'))
    .join('\n')
}

function viewSource(name: string): string {
  return readFileSync(join(viewsDir, name), 'utf8')
}

describe('priority display: one owner, rendered through it', () => {
  it('pins exactly the views that render a priority', () => {
    const found = readdirSync(viewsDir)
      .filter((f) => f.endsWith('.vue'))
      .filter((f) => PRIORITY_INTERPOLATION.test(scriptCode(viewSource(f))))
      .sort()
    expect(found).toEqual(Object.keys(PRIORITY_VIEWS).sort())
  })

  it('each declared view imports the helper and renders its expression through it', () => {
    for (const [name, expression] of Object.entries(PRIORITY_VIEWS)) {
      const source = viewSource(name)
      expect(IMPORTS_HELPER.test(source), `${name} must import priorityLabel from utils/format`).toBe(true)
      expect(source, `${name} must render ${expression} via priorityLabel`).toContain(
        `priorityLabel(${expression})`,
      )
    }
  })

  it('every line that puts a priority on screen routes it through the helper', () => {
    for (const [name, expression] of Object.entries(PRIORITY_VIEWS)) {
      const offenders = scriptCode(viewSource(name))
        .split('\n')
        .map((line) => line.trim())
        .filter((line) => PRIORITY_INTERPOLATION.test(line))
        .filter((line) => !line.includes('priorityLabel('))
      expect(offenders, `${name} interpolates a priority (${expression}) without labeling it`).toEqual([])
    }
  })

  it('the instantiate wizard offers the vocabulary from its owner, not from copy-paste', () => {
    const source = viewSource('TemplatesView.vue')
    expect(source, 'the select must iterate the declared list').toContain('v-for="p in PRIORITIES"')
    // The deleted shape: four literal option lines. A fifth priority added on the
    // server would be silently absent from a hand-copied list.
    for (const spelling of ['low', 'normal', 'high', 'urgent']) {
      expect(source, `the wizard must not hard-code value="${spelling}"`).not.toContain(`value="${spelling}"`)
    }
  })

  it('the wizard form type carries the vocabulary instead of an open string', () => {
    expect(readFileSync(templateFormPath, 'utf8')).toContain('priority: Priority')
  })

  it('utils/format still owns both tables, the runtime list and the label helper', () => {
    const source = readFileSync(formatPath, 'utf8')
    const exported = (name: string): boolean =>
      new RegExp(`export (const|function) ${name}\\b`).test(source)
    for (const name of ['PRIORITY_LABEL', 'PRIORITY_COLOR', 'PRIORITIES', 'priorityLabel']) {
      expect(exported(name), `${name} must stay exported from utils/format`).toBe(true)
    }
  })
})
