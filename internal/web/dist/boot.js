/* Anti-flash bootstrap — served as a REAL FILE (web/public/boot.js), not an
 * inline script, and that is load-bearing: the embedded console's server
 * (internal/web/embed.go) ships `Content-Security-Policy: default-src 'self'`,
 * which forbids inline scripts. An inline version of this file was silently
 * refused by the browser and the console still booted (useTheme/useDensity
 * re-apply on import — they are designed not to depend on this script), but
 * the first frame painted unthemed. As an external same-origin file it runs
 * under 'self' and the anti-flash guarantee is real again.
 *
 * This runs before first paint and does what useTheme/useDensity do: read the
 * stored choices, resolve them, set the classes. It necessarily duplicates
 * the storage keys, media query and class names from
 * web/src/composables/useTheme.ts and useDensity.ts (this runs before the
 * module graph loads); useTheme.spec.ts and useDensity.spec.ts pin both ends
 * so the copies cannot drift silently.
 */
;(function () {
  try {
    var stored = localStorage.getItem('levee-theme')
    var dark =
      stored === 'dark' ||
      (stored !== 'light' &&
        window.matchMedia('(prefers-color-scheme: dark)').matches)
    if (dark) {
      document.documentElement.classList.add('dark')
      document.documentElement.style.colorScheme = 'dark'
    } else {
      document.documentElement.style.colorScheme = 'light'
    }
  } catch (e) {
    /* localStorage unavailable: fall through to the light default. */
  }
  try {
    /* Density is applied in the same bootstrap and for the same reason:
     * the compact overrides change panel and row heights, so applying it
     * from the app would paint one frame at the comfortable rhythm and
     * then jump — visible as a reflow on every navigation, and worse on
     * the map, whose scrollbar is computed from the row pitch. Key and
     * class duplicated from useDensity.ts on purpose (see header). */
    if (localStorage.getItem('levee-density') === 'compact') {
      document.documentElement.classList.add('density-compact')
    }
  } catch (e) {
    /* localStorage unavailable: fall through to the comfortable default. */
  }
})()
