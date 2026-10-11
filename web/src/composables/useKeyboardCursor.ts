// KeyboardCursor — the row-level keyboard path the roadmap's P1 asks for
// ("审批 + 详情页的操作链应能在不开鼠标的情况下完成").
//
// Element Plus tables are mouse-shaped: rows navigate on click, and the only
// keyboard way to reach a row's buttons is to Tab through every cell before
// it. A Tab path technically completes the chain but an operator approving
// twenty changes does not want twenty Tab presses per row. So the cursor is
// EXPLICIT: j/k move a visible row cursor (the same shape Gmail-style list
// navigation uses), Enter opens the row, and the view maps its own letters to
// row actions (a=approve, r=reject …). The visible cursor is the point — a
// hidden keyboard mode that changes what keys do with no on-screen affordance
// is indistinguishable from a broken page.
//
// Fail-closed rules, in order:
//
//   1. Modified combos (Ctrl/Cmd/Alt) are left to the application — the shell
//      already binds Ctrl+K for the palette and must keep it.
//   2. A typing target (input / textarea / select / contenteditable) keeps its
//      keys: pressing j in a filter box must type a j, not scroll a cursor.
//   3. An open Element Plus overlay (dialog, message box) keeps its own keys:
//      the confirm box after "approve" is where Enter belongs, and firing a
//      second approve under it from the page underneath is the failure mode
//      this rule exists to prevent.
//
// Cleanup on unmount is not optional: a keydown listener bound to window keeps
// firing after the view navigated away, and its row refs point at data that no
// longer exists.
import { onUnmounted, ref } from 'vue'

export interface KeyboardCursorOptions {
  /** Reactive row count; the cursor clamps to it after a filter. */
  count: () => number
  /** Enter / o on the cursor row. */
  onOpen?: (index: number) => void
  /** Lowercase letter → action on the cursor row. */
  actions?: Record<string, (index: number) => void>
  /** Extra gate; when it returns false the keys do nothing. */
  enabled?: () => boolean
}

/**
 * True when the event came from a text-entry surface. Tag-based rather than
 * :focus-within so a keyup inside a dialog's comment box is also a typing
 * target.
 */
export function isTypingTarget(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null
  if (!el || typeof el.tagName !== 'string') return false
  const tag = el.tagName.toLowerCase()
  if (tag === 'input' || tag === 'textarea' || tag === 'select') return true
  return el.isContentEditable === true
}

/**
 * True while an Element Plus overlay owns the screen (dialog, message box,
 * drawer). These trap their own keys, so the page's cursor must stand down.
 */
export function hasOpenOverlay(): boolean {
  if (typeof document === 'undefined') return false
  return document.querySelector('.el-overlay') !== null
}

export function useKeyboardCursor(options: KeyboardCursorOptions): {
  index: ReturnType<typeof ref<number>>
  reset: () => void
  stop: () => void
} {
  // -1 means "no cursor yet": the page renders its normal self until the
  // operator opts in with j/k, so a page that never uses the keyboard looks
  // exactly as it did before.
  const index = ref(-1)

  function clamp(next: number): number {
    const n = options.count()
    if (n <= 0) return -1
    return Math.max(0, Math.min(next, n - 1))
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.ctrlKey || event.metaKey || event.altKey) return
    if (isTypingTarget(event.target)) return
    if (hasOpenOverlay()) return
    if (options.enabled && !options.enabled()) return

    const key = event.key.toLowerCase()
    if (key === 'j' || key === 'arrowdown') {
      index.value = index.value < 0 ? 0 : clamp(index.value + 1)
      event.preventDefault()
      return
    }
    if (key === 'k' || key === 'arrowup') {
      // Caret semantics: the first k from "no cursor" lands on the LAST row,
      // which is the one closest to the operator's hand on a long list.
      index.value = index.value < 0 ? clamp(options.count() - 1) : clamp(index.value - 1)
      event.preventDefault()
      return
    }
    if (index.value < 0) return
    if (key === 'enter' || key === 'o') {
      options.onOpen?.(index.value)
      event.preventDefault()
      return
    }
    const action = options.actions?.[key]
    if (action) {
      action(index.value)
      event.preventDefault()
    }
  }

  // Detach defensively when the caller is not a component (tests, and any
  // future imperative use); inside a setup() the onUnmounted below already
  // does it.
  const stop = (): void => window.removeEventListener('keydown', onKeydown)

  window.addEventListener('keydown', onKeydown)
  onUnmounted(() => stop())

  return {
    index,
    reset: () => {
      index.value = -1
    },
    stop,
  }
}
