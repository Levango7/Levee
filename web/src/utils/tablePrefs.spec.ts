import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  prefKey,
  readPrefs,
  sanitize,
  toggleColumn,
  writePrefs,
  type TablePrefs,
} from './tablePrefs'

const KNOWN = ['label', 'status', 'scope', 'template', 'created'] as const
const DEFAULTS = ['label', 'status', 'scope', 'template', 'created'] as const

// jsdom gives a working localStorage; tests clear it so each case starts from
// "nothing saved".
beforeEach(() => {
  window.localStorage.clear()
})

describe('sanitize', () => {
  it('keeps only known names, in canonical order', () => {
    expect(sanitize(['created', 'label', 'nope'], KNOWN)).toEqual(['label', 'created'])
  })

  it('drops duplicates and non-strings', () => {
    expect(sanitize(['label', 'label', 7, null, 'status'], KNOWN)).toEqual(['label', 'status'])
  })

  it('returns empty for anything that is not a list', () => {
    expect(sanitize('label', KNOWN)).toEqual([])
    expect(sanitize(null, KNOWN)).toEqual([])
    expect(sanitize(undefined, KNOWN)).toEqual([])
  })
})

describe('toggleColumn', () => {
  it('hides a visible column', () => {
    const r = toggleColumn(['label', 'status'], 'status', KNOWN)
    expect(r.columns).toEqual(['label'])
    expect(r.refused).toBe(false)
  })

  it('refuses to hide the last visible column', () => {
    // A table with no columns is a blank page that reads as "no data" — the
    // exact conflation this console keeps fixing.
    const r = toggleColumn(['label'], 'label', KNOWN)
    expect(r.columns).toEqual(['label'])
    expect(r.refused).toBe(true)
  })

  it('restores a hidden column in canonical order, not at the end', () => {
    const r = toggleColumn(['label', 'created'], 'status', KNOWN)
    expect(r.columns).toEqual(['label', 'status', 'created'])
  })

  it('ignores an unknown column', () => {
    const r = toggleColumn(['label'], 'nope' as (typeof KNOWN)[number], KNOWN)
    expect(r.columns).toEqual(['label'])
    expect(r.refused).toBe(false)
  })
})

describe('readPrefs / writePrefs', () => {
  it('falls back when nothing is saved', () => {
    expect(readPrefs('changes', KNOWN, DEFAULTS).columns).toEqual([...DEFAULTS])
  })

  it('round-trips what was written', () => {
    const prefs: TablePrefs<(typeof KNOWN)[number]> = { columns: ['label', 'status'], filter: 'pay' }
    writePrefs('changes', prefs)
    expect(readPrefs('changes', KNOWN, DEFAULTS)).toEqual({ columns: ['label', 'status'], filter: 'pay' })
  })

  it('drops stale column names from an older build', () => {
    window.localStorage.setItem(
      prefKey('changes'),
      JSON.stringify({ columns: ['label', 'gone_in_v2'], filter: 7 }),
    )
    const got = readPrefs('changes', KNOWN, DEFAULTS)
    expect(got.columns).toEqual(['label'])
    expect(got.filter).toBeUndefined()
  })

  it('falls back when the stored value is not JSON', () => {
    window.localStorage.setItem(prefKey('changes'), '{not json')
    expect(readPrefs('changes', KNOWN, DEFAULTS).columns).toEqual([...DEFAULTS])
  })

  it('falls back when the stored column list is empty', () => {
    // An explicitly-empty list means "show nothing", which is the blank-page
    // state; a preference that says that is treated as no preference.
    window.localStorage.setItem(prefKey('changes'), JSON.stringify({ columns: [] }))
    expect(readPrefs('changes', KNOWN, DEFAULTS).columns).toEqual([...DEFAULTS])
  })

  it('namespaces keys per table', () => {
    writePrefs('changes', { columns: ['label'] })
    writePrefs('targets', { columns: ['status'] })
    expect(readPrefs('changes', KNOWN, DEFAULTS).columns).toEqual(['label'])
    expect(readPrefs('targets', KNOWN, DEFAULTS).columns).toEqual(['status'])
  })

  it('survives a storage that throws (private mode)', () => {
    // Safari private mode and some embedded webviews throw on access; a
    // preference is never worth an exception at boot.
    const real = window.localStorage.getItem.bind(window.localStorage)
    vi.spyOn(window.localStorage, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError')
    })
    expect(readPrefs('changes', KNOWN, DEFAULTS).columns).toEqual([...DEFAULTS])
    expect(() => writePrefs('changes', { columns: ['label'] })).not.toThrow()
    vi.restoreAllMocks()
    expect(real).toBeTypeOf('function')
  })
})
