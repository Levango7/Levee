// dist-boot-check.mjs — load the BUILT dist in a real browser and assert the
// app actually boots.
//
// WHY THIS EXISTS: every gate this repo had measured the DEV server, where
// vite does not chunk. The one class of defect that class of gate cannot see
// is a broken BUILT bundle — and it happened for real: rolldown 1.2.15 under
// a manualChunks config emitted a call to its `__exportAll` ESM runtime
// helper without emitting the helper, so every production build white-
// screened at boot ("__exportAll is not a function", vendor chunk, before
// the app mounted) while dev, vitest and the dev-server browser checks all
// stayed green (2026-11-21, caught by the real-backend probe only).
//
// This check is the missing gate shape: serve the freshly built dist with
// `vite preview` (the CI step owns the server lifecycle, same one-shell
// pattern as the browser-check job) and assert, in a real browser:
//   1. the SPA shell mounts (#app has children — the white-screen signature
//      is appChildren 0),
//   2. no uncaught page error,
//   3. the shell's own text painted,
//   4. a deep link (/cluster) also boots — the SPA fallback path,
//   5. no asset script failed to load (chunk-reference drift).
//
// Server lifecycle belongs to the caller (CI step or local runner). The
// script only needs BASE_URL (default http://127.0.0.1:4173) and a browser.
// Skips with exit 0 when no browser/driver is available, unless
// BROWSER_CHECK_REQUIRE=1 — the same skip-and-tell contract browser-check
// uses, so CI can require it and local runs stay optional.

import { execSync } from 'node:child_process'
import { existsSync } from 'node:fs'
import { createRequire } from 'node:module'
import { pathToFileURL } from 'node:url'

const BASE = process.env.BASE_URL || 'http://127.0.0.1:4173'
const REQUIRED = process.env.BROWSER_CHECK_REQUIRE === '1'
const TIMEOUT = Number(process.env.BOOT_CHECK_TIMEOUT_MS || 30_000)

// Browser resolution mirrors scripts/browser-check.mjs: explicit CHROME_PATH,
// then the well-known Linux/Windows locations, then PATH. Chrome first;
// Firefox's launcher needs different flags and boot-failure modes are not
// engine-specific — one engine is enough for this gate.
const CHROME_CANDIDATES = [
  process.env.CHROME_PATH,
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
].filter(Boolean)

function findChrome() {
  for (const p of CHROME_CANDIDATES) {
    if (p && existsSync(p)) return p
  }
  try {
    execSync('where chrome', { stdio: 'ignore' }) // Windows PATH lookup
    return 'chrome'
  } catch { /* none */ }
  return null
}

async function loadPuppeteer() {
  const require = createRequire(import.meta.url)
  const candidates = []
  try {
    const globalRoot = execSync('npm root -g', { encoding: 'utf8' }).trim().replace(/\\/g, '/')
    // npm root -g IS the node_modules dir; require.resolve needs a directory
    // to resolve FROM, so hand it the global root itself.
    candidates.push(globalRoot)
  } catch { /* npm not on PATH */ }
  for (const p of (process.env.NODE_PATH || '').split(require('node:path').delimiter)) {
    if (p) candidates.push(p.replace(/\\/g, '/'))
  }
  candidates.push(process.cwd())
  for (const root of candidates) {
    try {
      return await import(pathToFileURL(require.resolve('puppeteer-core', { paths: [root] })).href)
    } catch { /* try the next root */ }
  }
  return null
}

const puppeteer = await loadPuppeteer()
if (!puppeteer) {
  const msg = 'dist-boot-check: puppeteer-core not found (global npm root, NODE_PATH, cwd)'
  if (REQUIRED) { console.error(msg); process.exit(1) }
  console.log(msg + ' — skipping')
  process.exit(0)
}
const chrome = findChrome()
if (!chrome) {
  const msg = 'dist-boot-check: no chrome found'
  if (REQUIRED) { console.error(msg); process.exit(1) }
  console.log(msg + ' — skipping')
  process.exit(0)
}

let pass = 0, fail = 0
const check = (label, ok, extra = '') => {
  if (ok) { pass++; console.log('  ok  ', label) } else { fail++; console.log('  FAIL', label, extra) }
}

const browser = await puppeteer.default.launch({
  executablePath: chrome,
  timeout: 60_000,
  args: ['--no-sandbox', '--headless=new', '--disable-dev-shm-usage'],
})
try {
  const page = await browser.newPage()
  await page.setViewport({ width: 1440, height: 1000 })
  const pageErrors = []
  page.on('pageerror', (e) => pageErrors.push(String(e).slice(0, 200)))
  const failedAssets = []
  page.on('response', (res) => {
    if (res.status() >= 400 && /\.m?js($|\?)/.test(res.url())) {
      failedAssets.push(`${res.status()} ${res.url().slice(-60)}`)
    }
  })

  // --- 1. the shell route boots -------------------------------------------
  await page.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: TIMEOUT })
  await page.waitForFunction(() => document.querySelector('#app') && document.querySelector('#app').children.length > 0, {
    timeout: 15_000,
  }).catch(() => {})
  const boot = await page.evaluate(() => ({
    children: document.querySelector('#app') ? document.querySelector('#app').children.length : 0,
    text: document.body.innerText.replace(/\s+/g, ' ').slice(0, 120),
  }))
  check('the built bundle mounts (#app has children)', boot.children > 0, JSON.stringify(boot))
  check('the shell text painted', boot.text.includes('LEVEE'), JSON.stringify(boot.text))
  check('no uncaught page error', pageErrors.length === 0, pageErrors.join(' | '))

  // --- 2. a deep link boots through the SPA fallback ----------------------
  // The router guard may bounce an unauthenticated visit to /login — that is
  // a ROUTE change inside a mounted app, exactly as good for this gate: the
  // assertion is "the bundle mounted and no error fired", not which screen.
  await page.goto(BASE + '/cluster', { waitUntil: 'domcontentloaded', timeout: TIMEOUT })
  await page.waitForFunction(() => document.querySelector('#app') && document.querySelector('#app').children.length > 0, {
    timeout: 15_000,
  }).catch(() => {})
  const deep = await page.evaluate(() => document.querySelector('#app') ? document.querySelector('#app').children.length : 0)
  check('deep link /cluster also boots (SPA fallback)', deep > 0, `children=${deep}`)
  check('deep link fired no uncaught error', pageErrors.length === 0, pageErrors.join(' | '))

  // --- 3. every chunk request succeeded -----------------------------------
  check('no failed script asset', failedAssets.length === 0, failedAssets.join(' | '))

  console.log(`\ndist-boot-check: ${pass} pass, ${fail} fail`)
  process.exit(fail ? 1 : 0)
} finally {
  await browser.close()
}
