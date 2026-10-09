// Unit tests for the theme state machine. jsdom gives us localStorage and a
// stubbable matchMedia, so all three states and the persistence contract can be
// exercised without a browser — which matters here because "system" is the
// state that is easiest to break silently (it looks identical to "light" until
// the OS flips).
//
// The pure helpers are imported statically; useTheme is re-imported per test,
// because the module is a deliberate singleton — it binds the media listener
// once and holds the choice at module scope, exactly as it should in a browser
// with one window. Re-importing gives each test its own instance instead of
// asking production code to carry a reset hook it never needs.

import { beforeEach, describe, expect, it, vi } from 'vitest'

import { DARK_MEDIA_QUERY, THEME_STORAGE_KEY, parseThemeChoice, resolveTheme } from './useTheme'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

type ThemeModule = typeof import('./useTheme')

/** installMatchMedia stubs window.matchMedia with a controllable query and
 *  returns the listener registry, so a test can fire "the OS switched". */
function installMatchMedia(matches: boolean): {
  listeners: Array<() => void>
  set: (v: boolean) => void
} {
  const listeners: Array<() => void> = []
  let value = matches
  const mql = {
    get matches() {
      return value
    },
    media: DARK_MEDIA_QUERY,
    addEventListener: (_: string, cb: () => void) => {
      listeners.push(cb)
    },
    removeEventListener: () => {},
    addListener: (cb: () => void) => {
      listeners.push(cb)
    },
    onchange: null,
    dispatchEvent: () => false,
  }
  vi.stubGlobal('matchMedia', () => mql)
  return {
    listeners,
    set: (v: boolean) => {
      value = v
    },
  }
}

/** freshTheme loads a fresh copy of the module (see the file header).
 *  Its suites take the owned cold-import timeout: the re-import runs the
 *  same cold transform the api specs hit (src/test/coldImport.ts). */
async function freshTheme(): Promise<ThemeModule['useTheme']> {
  vi.resetModules()
  const mod: ThemeModule = await import('./useTheme')
  return mod.useTheme
}

beforeEach(() => {
  window.localStorage.clear()
  document.documentElement.className = ''
  document.documentElement.style.colorScheme = ''
  vi.unstubAllGlobals()
})

describe('parseThemeChoice', () => {
  it('accepts the three known choices', () => {
    expect(parseThemeChoice('light')).toBe('light')
    expect(parseThemeChoice('dark')).toBe('dark')
    expect(parseThemeChoice('system')).toBe('system')
  })

  it('falls back to system for anything else, including null', () => {
    expect(parseThemeChoice(null)).toBe('system')
    expect(parseThemeChoice('')).toBe('system')
    expect(parseThemeChoice('solarized')).toBe('system')
    expect(parseThemeChoice('DARK')).toBe('system')
  })
})

describe('resolveTheme', () => {
  it('resolves system against the media query', () => {
    expect(resolveTheme('system', true)).toBe('dark')
    expect(resolveTheme('system', false)).toBe('light')
  })

  it('lets an explicit choice override the media query', () => {
    expect(resolveTheme('light', true)).toBe('light')
    expect(resolveTheme('dark', false)).toBe('dark')
  })
})

describe('useTheme', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  // The bootstrap in index.html necessarily duplicates these two constants
  // (it runs before the module graph loads). Pin them so renaming either one
  // turns this red instead of silently making the stored choice unreadable.
  it('pins the storage key and media query the inline bootstrap reads', () => {
    expect(THEME_STORAGE_KEY).toBe('levee-theme')
    expect(DARK_MEDIA_QUERY).toBe('(prefers-color-scheme: dark)')
  })

  it('starts from the stored choice', async () => {
    installMatchMedia(false)
    window.localStorage.setItem(THEME_STORAGE_KEY, 'dark')
    const useTheme = await freshTheme()

    const theme = useTheme()
    expect(theme.choice.value).toBe('dark')
    expect(theme.resolved.value).toBe('dark')
    // Importing the module must APPLY the stored theme, not merely remember it.
    // index.html applies it before first paint, but the app cannot depend on
    // that inline script having run (CSP, a cached shell) — and when the two
    // disagree, the console renders the stored theme's opposite and never
    // corrects itself until the user toggles.
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.style.colorScheme).toBe('dark')
  })

  it('corrects a document that the inline bootstrap got wrong', async () => {
    installMatchMedia(false)
    window.localStorage.setItem(THEME_STORAGE_KEY, 'dark')
    document.documentElement.classList.remove('dark') // what a stripped bootstrap leaves
    const useTheme = await freshTheme()

    useTheme()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('applies the resolved theme to <html> and persists explicit choices', async () => {
    installMatchMedia(false)
    const useTheme = await freshTheme()
    const theme = useTheme()

    theme.setChoice('dark')
    expect(document.documentElement.classList.contains('dark')).toBe(true)
    expect(document.documentElement.style.colorScheme).toBe('dark')
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('dark')

    theme.setChoice('light')
    expect(document.documentElement.classList.contains('dark')).toBe(false)
    expect(document.documentElement.style.colorScheme).toBe('light')
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe('light')
  })

  it('keeps following the OS while the choice is system', async () => {
    const media = installMatchMedia(false)
    const useTheme = await freshTheme()
    const theme = useTheme()

    theme.setChoice('system')
    expect(document.documentElement.classList.contains('dark')).toBe(false)

    // The OS flips at dusk: the console must follow without a reload.
    media.set(true)
    media.listeners.forEach((cb) => cb())
    expect(document.documentElement.classList.contains('dark')).toBe(true)

    // And an explicit choice stops the following, even when the OS flips again.
    theme.setChoice('light')
    media.set(true)
    media.listeners.forEach((cb) => cb())
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })

  it('toggles to the opposite of what is displayed, not to system', async () => {
    installMatchMedia(false)
    const useTheme = await freshTheme()
    const theme = useTheme()

    theme.setChoice('system') // resolves light
    theme.toggle()
    expect(theme.choice.value).toBe('dark')
    theme.toggle()
    expect(theme.choice.value).toBe('light')
  })
})
