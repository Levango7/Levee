// browser-check.mjs — the console's browser-level checks.
//
// WHY THIS EXISTS: the unit tests run without a response body and without a
// layout engine, so the behaviour that lives in REACTING to a payload has never
// been covered: a 403 that must not render as "no data", a trend whose legend
// order must match the stack, a day boundary that must follow the operator's
// clock, a 300-host map whose DOM must stay flat. Those are exactly the
// properties that regress silently — a screenshot would show them, but nobody
// takes screenshots in CI.
//
// So this is assertion-driven, not screenshot-driven: it drives a real Chrome
// against the dev server (wired to the committed mock gateway) and asserts DOM,
// geometry and behaviour. `--shots` additionally writes PNGs to
// web/.browser-shots/ for a human to look at.
//
// Usage:
//   npm run test:browser            # assertions only
//   npm run test:browser -- --shots # also write screenshots
//
// Requires Chrome at CHROME_PATH, or one of the well-known locations below
// (GitHub's ubuntu and windows runners both ship Chrome). BROWSER_CHECK_REQUIRE=1
// turns every "cannot run" into a failure — that is what CI sets, so a missing
// driver cannot silently pass a green build. Locally the default stays
// skip-and-tell: these are an additional gate, not a replacement for the unit
// tests.
import { spawn, execSync } from 'node:child_process'
import { existsSync, mkdirSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'
import { createRequire } from 'node:module'
import { pathToFileURL } from 'node:url'

// Candidate paths, in order. The Linux ones come first because CI runs there;
// the Windows default is this repo's own machine.
const CHROME_CANDIDATES = [
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
]

function resolveChrome() {
  // An explicitly-set path that does not exist is a configuration error, not a
  // fallback trigger: silently trying the next candidate would run the checks
  // against a DIFFERENT browser than the one that was asked for.
  if (process.env.CHROME_PATH) {
    if (existsSync(process.env.CHROME_PATH)) return process.env.CHROME_PATH
    console.error('browser-check: CHROME_PATH is set but does not exist: ' + process.env.CHROME_PATH)
    return ''
  }
  for (const p of CHROME_CANDIDATES) {
    if (existsSync(p)) return p
  }
  // `which` catches installs outside the well-known paths.
  for (const bin of ['google-chrome', 'chromium', 'chromium-browser', 'chrome']) {
    try {
      const found = execSync('which ' + bin + ' 2>/dev/null || where ' + bin + ' 2>nul', { encoding: 'utf8' })
        .trim()
        .split('\n')[0]
      if (found && existsSync(found.trim())) return found.trim()
    } catch {
      /* not on PATH */
    }
  }
  return ''
}

const REQUIRED = process.env.BROWSER_CHECK_REQUIRE === '1'
const CHROME = resolveChrome()
const BASE = process.env.BASE_URL || 'http://localhost:5173'
// The mock gateway's own address: the failure modes are switched by talking to
// it directly, because the page's origin is the dev server and /__mode exists
// only on the mock.
const MOCK = process.env.MOCK_URL || 'http://127.0.0.1:4599'
const SHOTS = process.argv.includes('--shots')
const SHOT_DIR = process.env.SHOT_DIR || 'web/.browser-shots'

// ESM ignores NODE_PATH, so the global install on this machine is resolved
// explicitly: the repo's own node_modules, then the global npm root. A machine
// with neither skips these checks rather than failing the suite — unless
// BROWSER_CHECK_REQUIRE=1, which is what CI sets.
const require = createRequire(import.meta.url)
// NODE_PATH is how this machine's global npm install is reachable; an explicit
// npm root -g is tried too, because NODE_PATH is not set by default and the
// check should still find a global install when there is one.
// Backslashes are normalised away: `npm root -g` prints a Windows path with
// them, and require.resolve's {paths} option does not accept that form (it
// resolves nothing) — forward slashes work on Windows.
// require.resolve's {paths} treats each entry as a directory to resolve FROM
// (like a file's own directory), so the global install's parent is what finds
// it: "E:/dev-data/npm-global/node_modules" resolves nothing, its parent does.
const normalise = (p) => p.split(String.fromCharCode(92)).join('/')
const parentOfNodeModules = (p) => (p.endsWith('/node_modules') ? p.slice(0, -'/node_modules'.length) : p)
const candidates = []
try {
  candidates.push(parentOfNodeModules(normalise(execSync('npm root -g', { encoding: 'utf8' }).trim())))
} catch {
  /* npm not on PATH */
}
for (const p of (process.env.NODE_PATH || '').split(require('node:path').delimiter)) {
  if (p) candidates.push(normalise(p))
}
candidates.push(process.cwd())

let puppeteer
for (const root of candidates) {
  try {
    const entry = require.resolve('puppeteer-core', { paths: [root] })
    // Windows absolute paths need a file:// URL before import() will take them
    // ("Received protocol 'e:'" otherwise).
    puppeteer = await import(pathToFileURL(entry).href)
    break
  } catch {
    /* try the next root */
  }
}
if (!puppeteer) {
  const where = 'looked in ' + candidates.join(', ')
  if (REQUIRED) {
    // CI sets BROWSER_CHECK_REQUIRE=1: a green build must not be able to mean
    // "nobody ran the browser checks".
    console.error('browser-check: puppeteer-core not found (' + where + ')')
    process.exit(1)
  }
  console.log('browser-check: puppeteer-core not found (' + where + ') — skipping')
  process.exit(0)
}

if (!CHROME) {
  const msg =
    'no Chrome found (tried CHROME_PATH, ' + CHROME_CANDIDATES.join(', ') + ', and PATH)'
  if (REQUIRED) {
    console.error('browser-check: ' + msg)
    process.exit(1)
  }
  console.log('browser-check: ' + msg + ' — skipping')
  process.exit(0)
}

const failures = []
const notes = []
function check(name, ok, detail) {
  if (ok) {
    console.log(`  ok   ${name}`)
  } else {
    console.log(`  FAIL ${name}${detail ? ' — ' + detail : ''}`)
    failures.push(name)
  }
}

async function newPage(browser, viewport = { width: 1600, height: 1000 }) {
  const page = await browser.newPage()
  await page.setViewport(viewport)
  await page.evaluateOnNewDocument(() => localStorage.setItem('levee.token', 'browser-check'))
  const errors = []
  page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)))
  page.on('console', (m) => {
    if (m.type() === 'error' && !m.text().includes('Failed to load resource')) errors.push(m.text().slice(0, 200))
  })
  page._errors = errors
  return page
}

async function open(page, path, settleMs = 900) {
  // domcontentloaded, never networkidle2: the monitor's SSE stream keeps a
  // connection open, so "idle" never arrives and a networkidle wait just burns
  // the timeout.
  await page.goto(BASE + path, { waitUntil: 'domcontentloaded', timeout: 30_000 })
  // The first page a fresh dev server renders is compiled on demand: the shell
  // can arrive before the view chunk, so a cold Vite serves a shell with an
  // empty content region. The warm-up call below pays that cost outside the
  // assertions.
  await sleep(settleMs)
}

// warmUp loads the shell once and waits until the FIRST VIEW HAS ACTUALLY
// RENDERED, then throws the page away. The compile cost of the first view
// lands here rather than inside the first assertion.
//
// It waits for content rather than sleeping a fixed time: on a slow runner
// (the windows CI box) Vite compiled /changes' chunk well beyond 3.5s, so the
// FIRST route of the run then failed with an empty h1 while every later route
// passed — the artefact looked like "two routes are broken" but it was one
// cold transform. Waiting for the h1 makes the warm-up cost the same
// everywhere.
async function warmUp(browser) {
  const page = await browser.newPage()
  await page.goto(BASE + '/changes', { waitUntil: 'domcontentloaded', timeout: 30_000 })
  const deadline = Date.now() + 60_000
  while (Date.now() < deadline) {
    const h1 = await page.evaluate(() => document.querySelector('.levee-page h1')?.textContent?.trim() || '')
    const rows = await page.evaluate(() => document.querySelectorAll('.el-table__row').length)
    if (h1 && rows > 0) break
    await sleep(500)
  }
  await page.close()
}

const text = (page, sel) => page.evaluate((s) => (document.querySelector(s)?.textContent || '').trim(), sel)

console.log('browser-check: launching Chrome')
const browser = await puppeteer.default.launch({
  executablePath: CHROME,
  args: ['--no-sandbox', '--headless=new', '--disable-dev-shm-usage', '--hide-scrollbars', '--force-color-profile=srgb'],
})

if (SHOTS && !existsSync(SHOT_DIR)) mkdirSync(SHOT_DIR, { recursive: true })

// ---------------------------------------------------------------------------
await warmUp(browser)

console.log('\n[1] every route renders its shell and its data')
const routes = [
  ['/changes', '变更看板', '.el-table__row'],
  ['/approval', '审批中心', '.el-table__row'],
  ['/monitor/run-1000', '执行监控', '.batch'],
  ['/audit', '审计查询', null],
  ['/cluster', '集群状态', null],   // node cards depend on the backend: see [7]
  ['/targets', '目标机管理', null],
  ['/templates', '模板管理', null],
  ['/system', '系统状态', null],
  ['/changes/run-1000', '变更详情', '.detail'],
  ['/not-a-page', '未找到', null],
]
for (const [path, title, rowSel] of routes) {
  const page = await newPage(browser)
  await open(page, path, 1200)
  const h1 = await text(page, '.levee-page h1, .levee-page h2, .not-found__title')
  const rows = rowSel ? await page.evaluate((s) => document.querySelectorAll(s).length, rowSel) : -1
  check(`${path} renders`, h1.length > 0 && page._errors.length === 0, `h1="${h1}" errors=${JSON.stringify(page._errors)}`)
  if (rowSel) check(`${path} has data rows`, rows > 0, `rows=${rows}`)
  if (SHOTS) await page.screenshot({ path: `${SHOT_DIR}/${path.replace(/\W+/g, '_')}.png` })
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[2] the three tiers: empty / failed / forbidden are distinct')
{
  const page = await newPage(browser)
  // ok
  await open(page, '/changes', 1500)
  const okRows = await page.evaluate(() => document.querySelectorAll('.el-table__row').length)
  check('normal mode shows rows', okRows > 0, `rows=${okRows}`)

  // forbidden: the region answers in place, and the table is not silently empty
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=forbidden')
  await open(page, '/changes', 1500)
  const forbiddenText = await text(page, '.state__title')
  check('403 renders the forbidden tier (not "no data")', forbiddenText.includes('没有权限'), `title="${forbiddenText}"`)

  // empty: a genuinely empty list is its own answer
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=empty')
  await open(page, '/changes', 1500)
  const emptyText = await text(page, '.state__title')
  const emptyHint = await text(page, '.state__hint')
  check('empty list renders the empty tier', emptyText.includes('没有数据'), `title="${emptyText}"`)
  check('the empty tier offers a next step', emptyHint.length > 0, `hint="${emptyHint}"`)

  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=ok')
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[3] the trend chart draws what the endpoint said')
{
  const page = await newPage(browser)
  await open(page, '/changes', 1600)
  const segs = await page.evaluate(() => document.querySelectorAll('.trend__seg').length)
  check('the chart draws stacked segments', segs > 20, `segments=${segs}`)
  const legend = await page.evaluate(() =>
    [...document.querySelectorAll('.trend__legend-item')].map((e) => e.textContent.trim()),
  )
  check('the legend is the five series in order',
    JSON.stringify(legend) === JSON.stringify(['成功', '进行中', '已回滚', '失败', '已中止']),
    JSON.stringify(legend))
  // Each segment must have a non-zero height: a bar with height 0 is a series
  // the CSS could not colour.
  const zero = await page.evaluate(() =>
    [...document.querySelectorAll('.trend__seg')].filter((r) => Number(r.getAttribute('height')) < 0.5).length)
  check('every segment has height', zero === 0, `zero-height=${zero}`)
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[4] the target map is windowed and its scrollbar is honest')
{
  const page = await newPage(browser)
  await open(page, '/monitor/run-fleet', 2600)
  const m = await page.evaluate(() => {
    const box = document.querySelector('.map-scroll')
    const rows = [...document.querySelectorAll('.map tbody tr')].filter((r) => !r.classList.contains('map__spacer'))
    const padTop = document.querySelectorAll('.map__spacer')[0].getBoundingClientRect().height
    const first = rows[0].querySelector('td').textContent
    const pitch = rows.length > 1
      ? Math.round(rows[1].getBoundingClientRect().top - rows[0].getBoundingClientRect().top)
      : 0
    return { rows: rows.length, scrollH: Math.round(box.scrollHeight), padTop: Math.round(padTop), first, pitch }
  })
  check('the map renders a window, not the fleet', m.rows > 0 && m.rows < 100, `rows=${m.rows}`)
  check('the scrollbar describes every host (300 x 27 + header)', Math.abs(m.scrollH - 8152) < 40, `scrollHeight=${m.scrollH}`)
  check('the row pitch is the measured 27px', m.pitch === 27, `pitch=${m.pitch}`)

  // Scroll to the bottom: the last host in the window must be the last row of
  // the sort, and the padding must have grown to match.
  const bottom = await page.evaluate(async () => {
    const box = document.querySelector('.map-scroll')
    box.scrollTop = box.scrollHeight
    await new Promise((r) => setTimeout(r, 300))
    const rows = [...document.querySelectorAll('.map tbody tr')].filter((r) => !r.classList.contains('map__spacer'))
    const spacers = document.querySelectorAll('.map__spacer')
    return {
      rows: rows.length,
      padTop: Math.round(spacers[0].getBoundingClientRect().height),
      padBottom: Math.round(spacers[1].getBoundingClientRect().height),
      last: rows[rows.length - 1].querySelector('td').textContent,
    }
  })
  check('scrolling to the bottom lands on the last rows', bottom.padBottom === 0, `padBottom=${bottom.padBottom}`)
  check('the window shrank to what is left', bottom.rows > 0 && bottom.rows < 100, `rows=${bottom.rows}`)
  if (SHOTS) await page.screenshot({ path: `${SHOT_DIR}/monitor_fleet.png` })
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[4b] the cluster page renders takeover settlements')
{
  const page = await newPage(browser)
  // The mock reports one still-interrupted run and one since resumed, so the
  // timeline must show BOTH statuses — a timeline that stamped the settlement
  // moment instead of the run's current status would report both as
  // interrupted, which is the bug internal/grpc/rest_takeover.go exists to
  // avoid.
  await open(page, '/cluster', 2200)
  const rows = await page.evaluate(() =>
    [...document.querySelectorAll('.takeover')].map((li) => ({
      run: li.querySelector('.takeover__run')?.textContent?.trim() || '',
      status: li.querySelector('.status__label')?.textContent?.trim() || '',
    })),
  )
  check('the takeover timeline renders both events', rows.length === 2, `rows=${JSON.stringify(rows)}`)
  const byRun = Object.fromEntries(rows.map((r) => [r.run, r.status]))
  check('a still-interrupted run reads as interrupted', byRun['run-1003'] === '已中断', JSON.stringify(byRun))
  check('a resumed run reads as its current status', byRun['run-1002'] === '执行中', JSON.stringify(byRun))
  if (SHOTS) await page.screenshot({ path: `${SHOT_DIR}/cluster_takeovers.png` })
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[5] the keyboard path moves a visible cursor and opens the row')
{
  const page = await newPage(browser)
  await open(page, '/changes', 1600)
  await page.keyboard.press('j')
  await sleep(200)
  const cursor = await page.evaluate(() => document.querySelectorAll('.lv-row-cursor').length)
  check('j paints a row cursor', cursor === 1, `cursors=${cursor}`)
  await page.keyboard.press('Enter')
  await sleep(900)
  const onDetail = page.url().includes('/changes/run-')
  check('Enter opens the cursor row', onDetail, `url=${page.url()}`)
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[6] the day boundary follows the operator, not the server')
{
  const page = await browser.newPage()
  await page.setViewport({ width: 1600, height: 1000 })
  // Install BEFORE the page loads: the request fires during mount, so an
  // interceptor attached afterwards observes nothing - and the check passes
  // for the wrong reason, or hangs, which is how this was found.
  // axios uses XMLHttpRequest in the browser, so open() is the transport to
  // watch, not fetch().
  await page.evaluateOnNewDocument(() => {
    window.__trendUrls = []
    const open = XMLHttpRequest.prototype.open
    XMLHttpRequest.prototype.open = function (m, u) {
      if (String(u).indexOf('change-trend') >= 0) window.__trendUrls.push(String(u))
      return open.apply(this, arguments)
    }
  })
  await open(page, '/changes', 2200)
  const seen = await page.evaluate(() => window.__trendUrls || [])
  const offset = /tz_offset=(-?[0-9]+)/
  check('the trend request carries tz_offset', seen.some((u) => offset.test(u)), JSON.stringify(seen))
  await page.close()
}

console.log('\n[7] the cluster page explains the deployment it is on')
{
  const page = await newPage(browser)
  // Default (SQLite): the page says coordination is off instead of showing an
  // empty node list. 'Nothing here' and 'this deployment has nothing to show'
  // are different answers, and conflating them is the bug this repo keeps fixing.
  await open(page, '/cluster', 1800)
  const hint = await text(page, '.hint')
  check('sqlite deployment explains that coordination is off',
    hint.indexOf('单节点') >= 0 || hint.indexOf('未启用') >= 0, 'hint=' + JSON.stringify(hint.slice(0, 60)))
  const cards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('and shows no fake node cards', cards === 0, 'cards=' + cards)

  // Cluster mode (PG backend): one card per registered node, and an offline
  // node coloured bad rather than ok.
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=cluster')
  await open(page, '/cluster', 1800)
  const clusterCards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('cluster deployment renders one card per node', clusterCards === 3, 'cards=' + clusterCards)
  const offline = await page.evaluate(() => document.querySelectorAll('.node .lv-dot--bad').length)
  check('an offline node is coloured bad, not ok', offline === 1, 'offline=' + offline)
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=ok')
  await page.close()
}

console.log(`\nbrowser-check: ${failures.length === 0 ? 'ALL PASS' : failures.length + ' FAILED'}`)
if (failures.length) {
  for (const f of failures) console.log('  - ' + f)
}
await browser.close()
process.exit(failures.length === 0 ? 0 : 1)
