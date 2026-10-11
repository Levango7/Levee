import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { hasOpenOverlay, isTypingTarget, useKeyboardCursor } from './useKeyboardCursor'

// The keyboard path is a GLOBAL listener, which makes its failure modes
// invisible in a component test and expensive in production: a key that fires
// while the operator types in a filter box, or a second "approve" that fires
// underneath an open confirm dialog. So the fail-closed rules are tested here
// with real events, not with a stubbed shape of a handler.
//
// useKeyboardCursor is called outside a component, so Vue logs its
// "onUnmounted is called when there is no active component instance" warning
// once per test; the listener itself registers before that line and the
// warning is not what is under test.
beforeEach(() => {
  vi.spyOn(console, 'warn').mockImplementation(() => {})
})
afterEach(() => {
  vi.restoreAllMocks()
})

function key(k: string, init: KeyboardEventInit = {}): void {
  window.dispatchEvent(new KeyboardEvent('keydown', { key: k, bubbles: true, ...init }))
}

describe('isTypingTarget', () => {
  it('lets text-entry surfaces keep their keys', () => {
    for (const tag of ['INPUT', 'TEXTAREA', 'SELECT']) {
      const el = document.createElement(tag)
      expect(isTypingTarget(el), tag).toBe(true)
    }
    const editable = document.createElement('div')
    // isContentEditable is read-only on the DOM type; jsdom honours the
    // definition, which is what the runtime predicate reads.
    Object.defineProperty(editable, 'isContentEditable', { value: true })
    expect(isTypingTarget(editable)).toBe(true)
  })

  it('does not swallow keys on buttons, rows or nothing at all', () => {
    const button = document.createElement('button')
    expect(isTypingTarget(button)).toBe(false)
    expect(isTypingTarget(document.createElement('div'))).toBe(false)
    expect(isTypingTarget(null)).toBe(false)
  })
})

describe('hasOpenOverlay', () => {
  it('stands down while a dialog owns the screen', () => {
    expect(hasOpenOverlay()).toBe(false)
    const overlay = document.createElement('div')
    overlay.className = 'el-overlay'
    document.body.appendChild(overlay)
    expect(hasOpenOverlay()).toBe(true)
    overlay.remove()
    expect(hasOpenOverlay()).toBe(false)
  })
})

// Every cursor mounted here detaches at the end of its test: the composable
// binds to window, and a listener that outlives its test would answer the next
// test's keystrokes too (the cross-talk the stop() return exists to prevent).
const mounted: Array<() => void> = []
afterEach(() => {
  while (mounted.length) mounted.pop()!()
})

function mountCursor(options: Parameters<typeof useKeyboardCursor>[0]) {
  const cursor = useKeyboardCursor(options)
  mounted.push(cursor.stop)
  return cursor
}

describe('useKeyboardCursor', () => {
  it('moves the cursor and clamps it to the row count', () => {
    const opened: number[] = []
    const { index, reset } = mountCursor({
      count: () => 3,
      onOpen: (i) => opened.push(i),
    })
    // No cursor until the operator opts in: the page looks normal until then.
    expect(index.value).toBe(-1)

    key('j')
    expect(index.value).toBe(0)
    key('j')
    key('j')
    key('j')
    expect(index.value, 'clamped to the last row').toBe(2)

    // k from "no cursor" lands on the LAST row (nearest the hand), then walks up.
    reset()
    expect(index.value).toBe(-1)
    key('k')
    expect(index.value).toBe(2)
    key('k')
    expect(index.value).toBe(1)
    key('j')
    expect(index.value).toBe(2)
  })

  it('opens the cursor row on Enter and on o', () => {
    const opened: number[] = []
    mountCursor({ count: () => 2, onOpen: (i) => opened.push(i) })
    key('j')
    key('j')
    key('Enter')
    key('o')
    expect(opened).toEqual([1, 1])
  })

  it('runs the page actions only on the cursor row', () => {
    const acted: Array<[string, number]> = []
    const { index } = mountCursor({
      count: () => 2,
      actions: {
        a: (i) => acted.push(['a', i]),
        r: (i) => acted.push(['r', i]),
      },
    })
    // With no cursor, a stray a must not act on row 0 by accident.
    key('a')
    expect(acted).toEqual([])

    key('j')
    key('a')
    key('r')
    expect(acted).toEqual([
      ['a', 0],
      ['r', 0],
    ])
    expect(index.value).toBe(0)
  })

  it('leaves modified combos to the application', () => {
    // The shell binds Ctrl/Cmd+K for the palette; the cursor must not also
    // consume those keys.
    const opened: number[] = []
    mountCursor({ count: () => 2, onOpen: (i) => opened.push(i) })
    key('j', { ctrlKey: true })
    key('j', { metaKey: true })
    expect(opened).toEqual([])
  })

  it('does nothing while an overlay is open', () => {
    // The confirm box that follows "approve" must own the keys: firing the
    // page action again from underneath it is the double-submit this prevents.
    const acted: string[] = []
    mountCursor({ count: () => 2, actions: { a: () => acted.push('a') } })
    const overlay = document.createElement('div')
    overlay.className = 'el-overlay'
    document.body.appendChild(overlay)
    key('j')
    key('a')
    expect(acted).toEqual([])
    overlay.remove()
    key('j')
    key('a')
    expect(acted).toEqual(['a'])
  })

  it('keeps a keystroke that came from a text field', () => {
    // isTypingTarget is unit-tested above; this pins the HANDLER path, where
    // the event arrives by bubbling from the focused input. A j typed in a
    // filter box must type a j, not move a cursor.
    const acted: string[] = []
    mountCursor({ count: () => 2, actions: { x: () => acted.push('x') } })
    const box = document.createElement('input')
    document.body.appendChild(box)
    box.focus()
    box.dispatchEvent(new KeyboardEvent('keydown', { key: 'x', bubbles: true }))
    expect(acted).toEqual([])
    // The same key anywhere else still reaches the handler (j opens the
    // cursor, x acts on it).
    key('j')
    key('x')
    expect(acted).toEqual(['x'])
    box.remove()
  })

  it('respects the enabled gate', () => {
    // The approval page enables its cursor only on the pending tab: the keys
    // must not act on a table the operator is not looking at.
    let enabled = false
    const opened: number[] = []
    mountCursor({
      count: () => 2,
      enabled: () => enabled,
      onOpen: (i) => opened.push(i),
    })
    key('j')
    key('Enter')
    expect(opened).toEqual([])
    enabled = true
    key('j')
    key('Enter')
    expect(opened).toEqual([0])
  })
})
