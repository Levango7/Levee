import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the row-level keyboard path (roadmap P1: "审批 + 详情页
// 的操作链应能在不开鼠标的情况下完成").
//
// The wiring has four properties that are easy to lose in an edit and expensive
// to notice by hand, so they are pinned here:
//
//   1. the cursor moves over the REAL row data (count() reads the same array
//      the table renders) — a cursor over a stale list acts on the wrong row;
//   2. the cursor is VISIBLE (row-class-name + the printed key hint) — a hidden
//      keyboard mode looks like a broken page to everyone who does not know it;
//   3. a reload or a tab switch resets it — Enter on a stale index opens the
//      wrong change;
//   4. the single owner of the fail-closed rules is the composable, so no view
//      re-implements the typing-target or overlay checks.

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

const locate = (rel: string): string => readFileSync(locatePath(rel), 'utf8')

const changesView = locate('src/views/ChangesView.vue')
const approvalView = locate('src/views/ApprovalView.vue')
const composable = locate('src/composables/useKeyboardCursor.ts')

function wiredViews(): Array<[string, string]> {
  // Views that call useKeyboardCursor. A new one joins this list or the
  // "cursor reset on reload" check below silently stops covering it.
  const dir = dirname(locatePath('src/views/ChangesView.vue'))
  return readdirSync(dir)
    .filter((f) => f.endsWith('.vue'))
    .map((f) => [f, readFileSync(join(dir, f), 'utf8')] as [string, string])
    .filter(([, src]) => src.includes('useKeyboardCursor'))
}

describe('keyboard cursor: the row path is wired and visible', () => {
  it('covers the two pages the roadmap names, and only those', () => {
    const wired = wiredViews().map(([name]) => name)
    expect(wired.sort()).toEqual(['ApprovalView.vue', 'ChangesView.vue'])
    expect(wired.length).toBeGreaterThan(0)
  })

  it('moves the cursor over the rows the table renders', () => {
    // count() must read the same array the table binds to, or the keys walk a
    // list the operator is not looking at.
    expect(changesView).toContain('count: () => changes.value.length')
    expect(approvalView).toContain('count: () => (activeTab.value === \'pending\' ? pending.value.length : 0)')
  })

  it('paints the cursor and prints the keys', () => {
    for (const [name, src] of wiredViews()) {
      // The cursor class must be applied from the row-class FUNCTION (script
      // half) and BOUND on the table (template half): a CSS rule for a class
      // nothing assigns paints nothing.
      const script = src.slice(0, src.indexOf('<template'))
      const template = src.slice(src.indexOf('<template'))
      expect(script, `${name} must apply the cursor class from code`).toContain('lv-row-cursor')
      expect(template, `${name} must bind the row class`).toContain(':row-class-name="')
      expect(src, `${name} must show the keys`).toContain('kbd-hint')
      expect(src, `${name} must style the cursor`).toContain('.lv-row-cursor')
    }
  })

  it('resets the cursor when its rows change underneath it', () => {
    // Enter on a stale index opens the wrong change; a filtered or reloaded
    // list is a different set of rows.
    for (const [name, src] of wiredViews()) {
      expect(src, `${name} must reset the cursor on reload`).toContain('cursor.reset()')
    }
    // The reset sits in the LOAD path, not only in a filter click handler: any
    // path that replaces the rows (pagination, the pager's @current-change, a
    // bulk action's applyFilter) must clear it.
    const loadFn = changesView.slice(
      changesView.indexOf('async function load('),
      changesView.indexOf('async function bulkPause') === -1
        ? changesView.length
        : changesView.indexOf('async function bulkPause'),
    )
    expect(loadFn, 'the reset must live inside load()').toContain('cursor.reset()')
    expect(loadFn).toContain('changes.value = res.items')
  })

  it('opens the detail page from the change list with Enter', () => {
    // The guarded form, not the bare call: the index is a keystroke away from a
    // list that may have been refetched, and Enter must not navigate to
    // whatever now occupies that row.
    expect(changesView).toContain('onOpen: (i) => {')
    expect(changesView).toContain('const row = changes.value[i]')
    expect(changesView).toContain('if (row) viewDetail(row)')
    // The keyboard path is the SAME navigation the row click uses, so the two
    // cannot drift into different destinations.
    expect(changesView).toContain('@row-click="viewDetail"')
  })

  it('drives the approval decisions and the chain from the keyboard', () => {
    for (const action of ['a', 'r', 'd']) {
      expect(approvalView, `action ${action}`).toContain(`${action}: (i) =>`)
    }
    // Every action re-resolves its row and refuses when the list moved: acting
    // on a stale index would approve the wrong change.
    expect(approvalView).toContain('function actOnRow(')
    expect(approvalView).toContain('if (!row) return')
    // Enter toggles the SAME expansion the mouse arrow does, through the table
    // instance's own API rather than a second local expansion state.
    expect(approvalView).toContain('pendingTable.value?.toggleRowExpansion(row)')
    expect(approvalView).toContain('ref="pendingTable"')
    expect(approvalView).toContain('type="expand"')
  })

  it('keeps the fail-closed rules in the single owner', () => {
    // A view that re-implements these (or binds its own window keydown) gets
    // the rules wrong the first time an input is focused.
    for (const [name, src] of wiredViews()) {
      expect(src, `${name} must not bind a raw window keydown`).not.toContain("addEventListener('keydown'")
    }
    // And the owner really checks them, in this order.
    expect(composable).toContain('event.ctrlKey || event.metaKey || event.altKey')
    expect(composable).toContain('isTypingTarget(event.target)')
    expect(composable).toContain('hasOpenOverlay()')
  })
})
