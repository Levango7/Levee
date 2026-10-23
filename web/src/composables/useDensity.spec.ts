// Unit tests for the density state machine. jsdom gives us localStorage, so the
// persistence contract and the document effect can be exercised without a
// browser.
//
// Two things here are worth more than the average unit test:
//
//   1. The inline bootstrap in index.html necessarily duplicates the storage key
//      and the class name (it runs before the module graph loads). The theme
//      suite pins the CONSTANTS and trusts the comment; this suite also reads
//      index.html and asserts the two spellings are actually present, so
//      renaming one end fails here instead of silently making a stored choice
//      unreadable.
//   2. "Default is comfortable" has to mean the class is ABSENT, not that some
//      other class is present — the whole no-regression claim for this feature
//      rests on the default render being the absence of the override.

import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  COMPACT_CLASS,
  DENSITY_STORAGE_KEY,
  applyDensity,
  parseDensityChoice,
} from './useDensity'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

type DensityModule = typeof import('./useDensity')

/** locatePath walks up from the working directory looking for `rel` (also
 *  trying it under web/), so the spec reads index.html whether vitest is started
 *  from the repo root or from web/ — the same helper wireContract.spec.ts uses,
 *  for the same reason: import.meta.url is not a file URL under vitest's
 *  transform, and a test that silently skipped this check would be worse than
 *  no check at all. */
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

/** freshDensity loads a fresh copy of the module: it is a deliberate singleton
 *  (module-scope ref + one document class), exactly as it should be in a browser
 *  with one window. Re-importing gives each test its own instance rather than
 *  asking production code to carry a reset hook it never needs. */
async function freshDensity(): Promise<DensityModule['useDensity']> {
  vi.resetModules()
  const mod: DensityModule = await import('./useDensity')
  return mod.useDensity
}

beforeEach(() => {
  window.localStorage.clear()
  document.documentElement.className = ''
  vi.unstubAllGlobals()
})

describe('parseDensityChoice', () => {
  it('accepts the two real choices and refuses to apply anything else', () => {
    expect(parseDensityChoice('comfortable')).toBe('comfortable')
    expect(parseDensityChoice('compact')).toBe('compact')
  })

  it('resolves an unknown value to comfortable rather than applying it verbatim', () => {
    // A value from an older build or a hand-edited localStorage must not
    // compress the console: comfortable is the shape those users already have.
    for (const raw of ['dense', '', null, undefined, 'COMPACT']) {
      expect(parseDensityChoice(raw)).toBe('comfortable')
    }
  })
})

describe('applyDensity', () => {
  it('toggles the one class, and comfortable means the class is ABSENT', () => {
    const root = document.createElement('div')

    applyDensity('compact', root)
    expect(root.classList.contains(COMPACT_CLASS)).toBe(true)

    applyDensity('comfortable', root)
    expect(root.classList.contains(COMPACT_CLASS)).toBe(false)
  })

  it('is idempotent', () => {
    const root = document.createElement('div')
    applyDensity('compact', root)
    applyDensity('compact', root)
    expect(root.className.split(/\s+/).filter((c) => c === COMPACT_CLASS)).toHaveLength(1)
  })
})

describe('useDensity', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  it('pins the key and class, and asserts index.html still spells them the same', () => {
    expect(DENSITY_STORAGE_KEY).toBe('levee-density')
    expect(COMPACT_CLASS).toBe('density-compact')

    // The other end of the duplication (see the file header).
    const html = readFileSync(locatePath('index.html'), 'utf8')
    expect(html).toContain(`localStorage.getItem('${DENSITY_STORAGE_KEY}')`)
    expect(html).toContain(`classList.add('${COMPACT_CLASS}')`)
  })

  it('defaults to comfortable and adds no class', async () => {
    const useDensity = await freshDensity()
    const density = useDensity()

    expect(density.choice.value).toBe('comfortable')
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(false)
  })

  it('starts from the stored choice and APPLIES it at import', async () => {
    window.localStorage.setItem(DENSITY_STORAGE_KEY, 'compact')
    const useDensity = await freshDensity()

    const density = useDensity()
    expect(density.choice.value).toBe('compact')
    // Importing must apply, not merely remember: index.html does it before first
    // paint, but the app cannot depend on that inline script having run.
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(true)
  })

  it('corrects a document the inline bootstrap got wrong', async () => {
    window.localStorage.setItem(DENSITY_STORAGE_KEY, 'compact')
    document.documentElement.classList.remove(COMPACT_CLASS) // a stripped bootstrap
    const useDensity = await freshDensity()

    useDensity()
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(true)
  })

  it('persists an explicit choice and applies it immediately', async () => {
    const useDensity = await freshDensity()
    const density = useDensity()

    density.setChoice('compact')
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(true)
    expect(window.localStorage.getItem(DENSITY_STORAGE_KEY)).toBe('compact')
    expect(density.isCompact()).toBe(true)

    density.setChoice('comfortable')
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(false)
    expect(window.localStorage.getItem(DENSITY_STORAGE_KEY)).toBe('comfortable')
  })

  it('survives a localStorage that throws (private mode, embedded webviews)', async () => {
    const thrower = {
      getItem() {
        throw new Error('denied')
      },
      setItem() {
        throw new Error('denied')
      },
    }
    vi.stubGlobal('localStorage', thrower)
    // With the global replaced, the module must still load and still behave.
    const useDensity = await freshDensity()
    const density = useDensity()

    expect(density.choice.value).toBe('comfortable')
    expect(() => density.setChoice('compact')).not.toThrow()
    expect(document.documentElement.classList.contains(COMPACT_CLASS)).toBe(true)
  })
})
