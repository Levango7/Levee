import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin, not a render test: the repo has no @vue/test-utils, so this
// page's one shipped defect — the batch panel sitting inside the
// `v-if="backend === 'sqlite'"` / `v-else-if` / `v-else` branch, and therefore
// never rendering in the default SQLite deployment — is guarded by asserting the
// shape of the template instead.
//
// It reads the source rather than a fixture so it cannot pass on a mock: if the
// block moves back inside the branch, this goes red.
//
// `import.meta.url` is not a file: URL under vite's transform, so the file is
// located from the working directory instead — and the search walks up so the
// guard reads the same whether vitest runs from web/ (npm script, CI) or from
// the repository root. Not finding it is a hard failure, never an empty string.
function locateSource(): string {
  const rel = ['src/views/ClusterView.vue', 'web/src/views/ClusterView.vue']
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of rel) {
      const p = join(dir, candidate)
      if (existsSync(p)) return readFileSync(p, 'utf8')
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ClusterView.vue from ${process.cwd()}`)
}

const source = locateSource()

const BATCH_HEADING = 'Run 批次进度'
const BRANCH_CLOSE = '\n\t\t</template>'
const SQLITE_BRANCH = `v-if="backend === 'sqlite'"`

/** Markup between two points, with HTML comments removed — this file's own
 *  explanatory comments quote `v-if="backend === 'sqlite'"`, and a guard that
 *  matched them would be asserting on prose. */
function codeBetween(from: number, to: number): string {
  return source.slice(from, to).replace(/<!--[\s\S]*?-->/g, '')
}

/** The <script> block with whole-line comments removed. This file quotes the
 *  expressions it retires (so a reader knows what was replaced), and a guard that
 *  matched its own prose would be asserting on comments. */
function scriptCode(): string {
  const start = source.indexOf('<script')
  const end = source.indexOf('</script>')
  if (start < 0 || end < 0) throw new Error('ClusterView.vue has no <script> block to read')
  return source
    .slice(start, end)
    .split('\n')
    .filter((line) => !line.trim().startsWith('//'))
    .join('\n')
}

describe('ClusterView node card vocabulary', () => {
  it('finds the node card anchors', () => {
    expect(source).toContain('v-for="n in nodes"')
    expect(source).toContain('node__role')
    expect(source).toContain("from '@/utils/node'")
  })

  it('names role and status through the shared vocabulary, not the wire value', () => {
    const card = source.slice(source.indexOf('v-for="n in nodes"'), source.indexOf('node__beat'))
    expect(card).toContain('nodeRoleLabel(n.role)')
    expect(card).toContain('nodeStatusLabel(n.status)')
    expect(card).toContain('nodeStatusTone(n.status)')
    // The two shapes this replaces: the raw role printed on a Chinese page, and the
    // raw status used as the dot's tooltip.
    expect(card).not.toMatch(/\{\{\s*n\.role\s*\}\}/)
    expect(card).not.toMatch(/:title="n\.status"/)
  })

  it('keeps the two-colour tone expression out of the view', () => {
    // `status === 'active' ? 'ok' : 'bad'` folded three registry states onto two
    // colours, so a gracefully leaving node was painted as an outage. The colour
    // decision now belongs to @/utils/node, where the Go guard can check it.
    const code = scriptCode()
    expect(code).not.toMatch(/function\s+nodeTone\b/)
    expect(code).not.toMatch(/'active'\s*\?\s*'ok'/)
  })
})

describe('ClusterView batch panel placement', () => {
  it('finds the anchors it is asserting on', () => {
    // Anchor honesty first: a renamed heading or a re-indented branch must fail
    // here, not silently satisfy the ordering assertions below.
    expect(source).toContain(BATCH_HEADING)
    expect(source).toContain(SQLITE_BRANCH)
    expect(source.split(BRANCH_CLOSE).length - 1).toBe(1)
    expect(source.indexOf(SQLITE_BRANCH)).toBeLessThan(source.indexOf(BRANCH_CLOSE))
  })

  it('renders the batch panel outside the backend/nodes branch', () => {
    const close = source.indexOf(BRANCH_CLOSE)
    const heading = source.indexOf(BATCH_HEADING)
    expect(heading).toBeGreaterThan(close)

    // And nothing backend-conditional re-wraps it: between the branch close and
    // the heading there may be no new conditional and no nested <template>.
    expect(codeBetween(close + BRANCH_CLOSE.length, heading)).not.toMatch(/v-if|v-else|<template/)
  })

  it('keeps the per-run query independent of the cluster summary payload', () => {
    // The panel is driven by selectedRunID/batchData, not by `data` (the
    // cluster-status response). If it ever gains a data-dependent guard the
    // SQLite regression comes back through a different door.
    const block = codeBetween(source.indexOf(BATCH_HEADING), source.indexOf(BATCH_HEADING) + 2200)
    expect(block).toContain('fetchBatchStatus')
    expect(block).not.toMatch(/v-if="!data\b|v-if="data\b|v-if="backend\b/)
  })
})
