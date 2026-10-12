// tablePrefs.ts — per-table column visibility and saved filter state.
//
// The console's tables have fixed columns: an operator who never looks at
// "模板" or "创建时间" cannot get them out of the way, and a filtered queue does
// not survive a reload — so coming back to "my pending prod changes" costs three
// clicks every time. Both are preferences, not features: they must degrade to a
// sane default when storage is unavailable or holds nonsense.
//
// Deliberately NOT here: drag-to-resize. Element Plus's table has no native
// column resize, and hand-rolling one means pointer capture, keyboard
// equivalents, RTL and touch handling for a problem the layout already answers
// (columns are min-width based with overflow tooltips). It is a separate
// decision, not a missing line in this file.
//
// Rules that are enforced here rather than trusted to callers:
//
//   - unknown names are dropped. A column renamed or removed must not resurrect
//     a stale preference from an older build (the same failure mode useTheme
//     guards against with a stored choice it no longer recognises).
//   - the last visible column cannot be hidden. A table with no columns is a
//     blank page that reads as "no data" — the exact conflation this console
//     keeps fixing.
//   - storage never throws. Private-mode Safari and some embedded webviews
//     throw on localStorage access, and a preference is never worth an
//     exception at boot.

/** Preferences for one table: which of its columns show, plus a saved filter. */
export interface TablePrefs<T extends string> {
  /** Visible columns, in display order. */
  columns: T[]
  /** Free-text filter the operator last applied. */
  filter?: string
}

/** Prefix every key: two tables' preferences must not collide. */
export function prefKey(table: string): string {
  return `levee.table.${table}`
}

/**
 * readPrefs loads and validates. Anything unparseable, unavailable or stale
 * resolves to `fallback` — a preference that cannot be read is a preference
 * that was never saved, and the table must still render.
 */
export function readPrefs<T extends string>(
  table: string,
  known: readonly T[],
  fallback: readonly T[],
): TablePrefs<T> {
  const defaults = sanitize(fallback, known)
  let raw: string | null = null
  try {
    raw = window.localStorage.getItem(prefKey(table))
  } catch {
    return { columns: defaults }
  }
  if (!raw) return { columns: defaults }
  try {
    const parsed = JSON.parse(raw) as { columns?: unknown; filter?: unknown }
    const cols = sanitize(parsed.columns, known)
    return {
      columns: cols.length > 0 ? cols : defaults,
      filter: typeof parsed.filter === 'string' ? parsed.filter : undefined,
    }
  } catch {
    return { columns: defaults }
  }
}

/** writePrefs persists, and reports nothing when it cannot (there is nothing
 *  the caller could do about it, and a console must not die over a pref). */
export function writePrefs<T extends string>(table: string, prefs: TablePrefs<T>): void {
  try {
    window.localStorage.setItem(prefKey(table), JSON.stringify(prefs))
  } catch {
    /* private mode / quota: preferences are best-effort by design */
  }
}

/**
 * sanitize keeps only known names, in the caller's canonical order, with
 * duplicates removed. An empty or invalid list returns empty — the caller
 * decides whether that means "fall back" or "nothing visible".
 */
export function sanitize<T extends string>(value: unknown, known: readonly T[]): T[] {
  if (!Array.isArray(value)) return []
  const allowed = new Set(known)
  const seen = new Set<string>()
  const out: T[] = []
  for (const item of value) {
    if (typeof item !== 'string' || !allowed.has(item as T) || seen.has(item)) continue
    seen.add(item)
    out.push(item as T)
  }
  // Canonical order: the table's own column order, so a stored preference from
  // an older build cannot permute the columns.
  return known.filter((k) => seen.has(k))
}

/**
 * toggleColumn flips one column's visibility, refusing to hide the last one.
 * The refusal is returned rather than thrown so the caller can tell the
 * operator why, instead of the click silently doing nothing.
 */
export function toggleColumn<T extends string>(
  current: readonly T[],
  column: T,
  known: readonly T[],
  minimum = 1,
): { columns: T[]; refused: boolean } {
  const cols = sanitize(current, known)
  if (cols.includes(column)) {
    if (cols.length <= minimum) return { columns: cols, refused: true }
    return { columns: cols.filter((c) => c !== column), refused: false }
  }
  return { columns: sanitize([...cols, column], known), refused: false }
}
