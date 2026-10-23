// useDensity — comfortable / compact vertical rhythm, persisted and applied.
//
// Why this exists: the console is an operations surface that people leave open
// all day, and the complaint that motivated it ("长期值守场景") is about how many
// rows fit on screen, not about taste. Density is therefore a *rhythm* switch,
// not a theme.
//
// What it deliberately is NOT: a rescaled design. The knobs below move the
// vertical rhythm of the LIST-shaped surfaces (tables, the target map, form
// controls). They do not multiply `--lv-space-*`, because every page composes
// those tokens — halving them would reflow layouts that were never measured at
// the smaller size, which is a different design rather than a denser one. The
// two levels share one type scale and one set of column widths on purpose: a
// column that survives at 27px per row must survive at 23px, and if it does not
// the fix belongs in that column, not in a second scale.
//
// There is no "system" option, unlike the theme: no operating system exposes a
// density preference to the browser, so inventing a third state would be a
// setting that can only ever mean one of the other two.
//
// The applied form is one class on <html> (`density-compact`), because that is
// where the CSS custom properties are overridden. "Comfortable" is the absence
// of the class, so the default rendering is byte-identical to what shipped
// before this module existed.

import { readonly, ref, type Ref } from 'vue'

export type DensityChoice = 'comfortable' | 'compact'

export const DENSITY_STORAGE_KEY = 'levee-density'

/** The class that switches the CSS overrides on. Exported so index.html's
 *  bootstrap and the tests agree on it rather than each spelling it. */
export const COMPACT_CLASS = 'density-compact'

const CHOICES: readonly DensityChoice[] = ['comfortable', 'compact']

/** parseDensityChoice narrows an arbitrary stored value. An unknown string (an
 *  older build, a hand-edited localStorage) resolves to comfortable rather than
 *  being applied verbatim — a bad value must not silently compress the console. */
export function parseDensityChoice(raw: string | null | undefined): DensityChoice {
  return CHOICES.includes(raw as DensityChoice) ? (raw as DensityChoice) : 'comfortable'
}

/** readStoredChoice is defensive: localStorage throws in private-mode Safari and
 *  in some embedded webviews, and a density preference is never worth an
 *  exception at boot. */
function readStoredChoice(): DensityChoice {
  try {
    return parseDensityChoice(window.localStorage.getItem(DENSITY_STORAGE_KEY))
  } catch {
    return 'comfortable'
  }
}

/** applyDensity writes the choice to the document. Idempotent and cheap, so the
 *  module can assert the state at load the way useTheme does — the app must not
 *  DEPEND on index.html's bootstrap having run (it can be stripped by a CSP or
 *  served from a stale cached document). */
export function applyDensity(next: DensityChoice, root?: Element): void {
  const el = root ?? document.documentElement
  el.classList.toggle(COMPACT_CLASS, next === 'compact')
}

// Module-level singleton: the shell header owns the control, but the map needs
// to read the pitch, and two components toggling must not fight over <html>.
const choice = ref<DensityChoice>(readStoredChoice())

applyDensity(choice.value)

/** useDensity returns the shared density state and its only mutator. */
export function useDensity(): {
  choice: Readonly<Ref<DensityChoice>>
  setChoice: (next: DensityChoice) => void
  isCompact: () => boolean
} {
  function setChoice(next: DensityChoice): void {
    choice.value = next
    applyDensity(next)
    try {
      window.localStorage.setItem(DENSITY_STORAGE_KEY, next)
    } catch {
      // Preference not persisted; the session still behaves correctly.
    }
  }

  return {
    choice: readonly(choice),
    setChoice,
    isCompact: () => choice.value === 'compact',
  }
}
