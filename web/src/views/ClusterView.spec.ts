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
const COORDINATION_BRANCH = 'v-if="answered && !clusterEnabled"'

/** Markup between two points, with HTML comments removed — this file's own
 *  explanatory comments quote `v-if="backend === 'sqlite'"`, and a guard that
 *  matched them would be asserting on prose. */
function codeBetween(from: number, to: number): string {
  return source.slice(from, to).replace(/<!--[\s\S]*?-->/g, '')
}

/** The <template> block with its HTML comments removed, for guards that have to
 *  know what the page renders rather than what its comments remember. The
 *  retired `backend === 'sqlite'` branch is quoted in two comments below, so
 *  asserting against the raw source could never see it go. */
function templateCode(): string {
  const start = source.indexOf('<template>')
  const end = source.lastIndexOf('</template>')
  if (start < 0 || end < 0 || end < start) throw new Error('ClusterView.vue has no <template> block to read')
  return source.slice(start, end).replace(/<!--[\s\S]*?-->/g, '')
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

describe('ClusterView capability filter', () => {
  // A capability filter turns "the list is empty" into two different facts:
  // nothing is registered, versus nothing declares what you asked for. The page
  // had one sentence for the empty case, so the second fact would have been
  // reported as an incident at the cluster instead of as a question about the
  // declaration. These guards keep the two sentences apart, and keep the page
  // honest about what the filter does NOT narrow (assignments: dispatch places
  // work by load, never by capability).

  const template = () =>
    source.slice(source.indexOf('<template>'), source.lastIndexOf('</template>')).replace(/<!--[\s\S]*?-->/g, '')

  it('asks the server to narrow, and tracks what was actually applied', () => {
    const markup = template()
    expect(markup).toContain('v-model="capabilityFilter"')
    expect(markup).toContain('@change="fetchStatus"')
    const code = scriptCode()
    expect(code).toContain('clusterStatus(')
    expect(code).toContain('appliedCapability.value')
    expect(code).toMatch(/systemApi\.clusterStatus\(want === '' \? undefined : \{ capability: want \}\)/)
  })

  it('does not read a filtered list as an empty registry', () => {
    const markup = template()
    const registryEmpty = markup.slice(
      markup.indexOf('v-else-if="answered && nodes.length === 0'),
    )
    // The condition itself must be guarded by the applied filter, or the two
    // facts collapse back into one branch.
    expect(registryEmpty.startsWith('v-else-if="answered && nodes.length === 0 && !appliedCapability"')).toBe(true)

    const noMatch = markup.slice(markup.indexOf('v-else-if="nodes.length === 0 && appliedCapability"'))
    expect(noMatch).not.toBe(markup)
    const noMatchBlock = noMatch.slice(0, noMatch.indexOf('</p>'))
    expect(noMatchBlock).toContain('没有节点声明能力')
    expect(noMatchBlock).not.toContain('未注册任何 worker 节点')
    expect(registryEmpty.slice(0, registryEmpty.indexOf('</p>'))).not.toContain('没有节点声明能力')
  })

  it('takes the filter from a deep link, not only from the box', () => {
    // The changes board already seeds its search from ?q=; a capability question
    // is worth sending as a link for the same reason. Seeding must happen BEFORE
    // the first fetch, or the page answers with the unfiltered registry and then
    // re-asks.
    const code = scriptCode()
    expect(code).toContain('useRoute')
    expect(code).toContain('route.query.capability')
    expect(code).toMatch(/typeof fromLink === 'string'/)
    const mounted = code.slice(code.indexOf('onMounted('))
    expect(mounted.indexOf('capabilityFilter.value')).toBeLessThan(mounted.indexOf('fetchStatus()'))
  })

  it('states that the assignment counts are not narrowed by the filter', () => {
    const markup = template()
    const caveat = markup.slice(markup.indexOf('v-if="appliedCapability"'))
    expect(caveat).not.toBe(markup)
    expect(caveat.slice(0, 400)).toContain('分配统计仍为全部署口径')
    // The wording that would imply the scheduler honours declarations.
    expect(caveat.slice(0, 400)).toContain('派发按负载，不按能力')
  })
})

describe('ClusterView batch panel placement', () => {
  it('finds the anchors it is asserting on', () => {
    // Anchor honesty first: a renamed heading or a re-indented branch must fail
    // here, not silently satisfy the ordering assertions below.
    expect(source).toContain(BATCH_HEADING)
    expect(source).toContain(COORDINATION_BRANCH)
    expect(source.split(BRANCH_CLOSE).length - 1).toBe(1)
    expect(source.indexOf(COORDINATION_BRANCH)).toBeLessThan(source.indexOf(BRANCH_CLOSE))
  })

  it('renders the batch panel outside the coordination/nodes branch', () => {
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
    expect(block).not.toMatch(/v-if="!data\b|v-if="data\b|v-if="backend\b|v-if="answered\b|v-if="!clusterEnabled\b/)
  })
})

describe('ClusterView coordination hints', () => {
	// The page used to answer "is this a cluster?" from `backend`, because that
	// was the only thing the wire reported. Two deployment shapes then shared one
	// sentence: a deliberate single-node PostgreSQL server was told to start
	// workers it never intended to run, and a cluster whose registry had been
	// emptied got the same reassurance. `cluster_enabled` separates them, and
	// these guards keep it that way.

	it('reads the coordination flag with the wire spelling', () => {
		const code = scriptCode()
		expect(code).toContain('cluster_enabled')
		// The camelCase twin is the #153 failure mode: a DTO that invents a
		// spelling reads undefined, and `undefined === true` is false, so the
		// page would announce "coordination off" for every cluster forever.
		expect(code).not.toMatch(/\.clusterEnabled\b/)
		expect(code).toMatch(/clusterEnabled\s*=\s*computed\(\(\)\s*=>\s*data\.value\?\.cluster_enabled/)
	})

	it('stops answering the coordination question with the storage label', () => {
		const markup = templateCode()
		expect(markup).not.toMatch(/backend\s*===\s*['"](sqlite|postgres)['"]/)
		expect(markup).toContain(COORDINATION_BRANCH)
	})

	// Shared anchor for the empty-registry branch. Three tests read this exact
	// condition, and they diverged once when #160 changed `backend === 'postgres'`
	// to `answered` and #161 appended `!appliedCapability` — one anchor, stated here.
	const REGISTRY_EMPTY = 'v-else-if="answered && nodes.length === 0 && !appliedCapability"'

	it('claims nothing about coordination before the server has answered', () => {
		// `data` starts null. A branch that keys only on !clusterEnabled would
		// state "coordination is disabled" during the first request and after a
		// failed one — the old page had the same shape of lie, told through the
		// `backend ?? 'unknown'` default.
		const markup = templateCode()
		expect(markup).toContain('v-if="answered && !clusterEnabled"')
		expect(markup).toContain(REGISTRY_EMPTY)
	})

	it('keeps the two shapes in separate sentences', () => {
		const markup = templateCode()
		// Anchor honesty: a missing branch makes indexOf return -1, slice(-1)
		// return one stray character, and then every assertion below would be
		// reading the whole page instead of one branch.
		expect(markup).toContain('v-if="answered && !clusterEnabled"')
		expect(markup).toContain(REGISTRY_EMPTY)
		const off = markup.slice(markup.indexOf('v-if="answered && !clusterEnabled"'))
		const empty = markup.slice(markup.indexOf(REGISTRY_EMPTY))
		expect(off.indexOf('</p>')).toBeGreaterThan(-1)
		expect(empty.indexOf('</p>')).toBeGreaterThan(-1)
		const offBlock = off.slice(0, off.indexOf('</p>'))
		const emptyBlock = empty.slice(0, empty.indexOf('</p>'))

		// Coordination off: says so, and does not send the operator off to start
		// workers. Coordination on with an empty registry: an incident, and
		// worded as one. Neither sentence may appear in the other's branch —
		// that merging is the defect this file exists for.
		expect(offBlock).toContain('未启用集群协调')
		expect(offBlock).not.toContain('节点注册表为空')
		expect(emptyBlock).toContain('节点注册表为空')
		expect(emptyBlock).not.toContain('未启用集群协调')
	})
})
