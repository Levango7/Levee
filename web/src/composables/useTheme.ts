// useTheme — light / dark / system theme state, persisted and applied.
//
// The applied form is a single `dark` class on <html>, because that is the hook
// Element Plus's own dark stylesheet (`html.dark`) and our token overrides both
// key on. Nothing else in the app reads the theme, so one class is the whole
// interface.
//
// "system" is a real third state, not a synonym for light: following the OS is
// what an operator expects on a workstation that switches at dusk, and it must
// keep following AFTER the user has visited a page once. That means storing the
// choice, not the resolved value, and re-resolving when the media query fires.
//
// index.html applies the stored choice before the bundle loads, so the first
// paint is already correct; this module owns every change after that.

import { readonly, ref, type Ref } from 'vue'

export type ThemeChoice = 'light' | 'dark' | 'system'

export const THEME_STORAGE_KEY = 'levee-theme'

/** Media query driving "system". Exported so index.html and tests agree on it. */
export const DARK_MEDIA_QUERY = '(prefers-color-scheme: dark)'

const CHOICES: readonly ThemeChoice[] = ['light', 'dark', 'system']

/** parseThemeChoice narrows an arbitrary stored value. An unknown string (an
 *  older build, a user-edited localStorage) resolves to "system" rather than
 *  being applied verbatim — a bad value must not pin the console to a theme. */
export function parseThemeChoice(raw: string | null | undefined): ThemeChoice {
  return CHOICES.includes(raw as ThemeChoice) ? (raw as ThemeChoice) : 'system'
}

/** resolveTheme turns a choice into the class the document should carry. */
export function resolveTheme(choice: ThemeChoice, prefersDark: boolean): 'light' | 'dark' {
  if (choice === 'system') return prefersDark ? 'dark' : 'light'
  return choice
}

/** readStoredChoice is defensive: localStorage throws in private-mode Safari
 *  and in some embedded webviews, and a theme preference is never worth an
 *  exception at boot. */
function readStoredChoice(): ThemeChoice {
  try {
    return parseThemeChoice(window.localStorage.getItem(THEME_STORAGE_KEY))
  } catch {
    return 'system'
  }
}

function prefersDarkNow(): boolean {
  return typeof window.matchMedia === 'function' && window.matchMedia(DARK_MEDIA_QUERY).matches
}

// Module-level singleton: the shell header owns the toggle, but any component
// may read the state, and two components toggling must not fight over <html>.
const choice = ref<ThemeChoice>(readStoredChoice())
const resolved = ref<'light' | 'dark'>(
  resolveTheme(choice.value, prefersDarkNow()),
)

let mediaBound = false

function applyToDocument(next: 'light' | 'dark'): void {
  document.documentElement.classList.toggle('dark', next === 'dark')
  // color-scheme drives native scrollbars and form controls; without it a dark
  // console keeps a white scrollbar gutter.
  document.documentElement.style.colorScheme = next
}

// Apply once at module load. index.html's inline bootstrap normally did this
// already (it runs before the bundle), but the console must not DEPEND on it:
// if that script is stripped by a CSP, served from a stale cached index.html,
// or the stored choice changes in another tab, the app would otherwise render
// the stored theme's opposite and never correct itself — the module owns the
// document state, so it asserts it here. Idempotent and cheap.
applyToDocument(resolved.value)

function bindMediaListener(): void {
  if (mediaBound || typeof window.matchMedia !== 'function') return
  mediaBound = true
  const mq = window.matchMedia(DARK_MEDIA_QUERY)
  const onChange = (): void => {
    if (choice.value !== 'system') return
    resolved.value = resolveTheme('system', mq.matches)
    applyToDocument(resolved.value)
  }
  // addEventListener is the modern form; older Safari exposes only
  // addListener. Guard rather than feature-detect twice at every call site.
  if (typeof mq.addEventListener === 'function') {
    mq.addEventListener('change', onChange)
  } else if (typeof mq.addListener === 'function') {
    mq.addListener(onChange)
  }
}

/** useTheme returns the shared theme state and its only mutator. */
export function useTheme(): {
  choice: Readonly<Ref<ThemeChoice>>
  resolved: Readonly<Ref<'light' | 'dark'>>
  setChoice: (next: ThemeChoice) => void
  toggle: () => void
} {
  bindMediaListener()

  function setChoice(next: ThemeChoice): void {
    choice.value = next
    resolved.value = resolveTheme(next, prefersDarkNow())
    applyToDocument(resolved.value)
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, next)
    } catch {
      // Preference not persisted; the session still behaves correctly.
    }
  }

  function toggle(): void {
    // Explicit light/dark on toggle: a user pressing the switch wants the
    // opposite of what they see, not a jump back to following the OS.
    setChoice(resolved.value === 'dark' ? 'light' : 'dark')
  }

  return {
    choice: readonly(choice),
    resolved: readonly(resolved),
    setChoice,
    toggle,
  }
}
