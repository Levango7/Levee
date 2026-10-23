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

// How long a page is allowed to take to reach a settled state. This is a
// deadline, never a wait: rendering is observed, and the deadline only bounds
// how long we are willing to observe. It is env-overridable so the wait can be
// proven load-bearing (BROWSER_CHECK_SETTLE_MS=1 must turn the suite red).
const SETTLE_DEADLINE_MS = Number(process.env.BROWSER_CHECK_SETTLE_MS || 30_000)
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
  await instrument(page)
  const errors = []
  page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)))
  page.on('console', (m) => {
    if (m.type() === 'error' && !m.text().includes('Failed to load resource')) errors.push(m.text().slice(0, 200))
  })
  page._errors = errors
  return page
}

// instrument counts the page's own REST calls so a wait can be judged on
// "the data has arrived" instead of "some milliseconds passed". It runs before
// any page script (evaluateOnNewDocument), so the requests fired during mount
// are counted — a counter installed afterwards observes nothing.
//
// Only /api traffic is counted. The monitor page holds an EventSource open for
// its SSE stream, and a wait that includes it never settles — which is the same
// reason open() below navigates on domcontentloaded and not networkidle2.
// axios uses XMLHttpRequest and the mock-mode switch uses fetch, so both
// transports are wrapped.
function instrument(page) {
  return page.evaluateOnNewDocument(() => {
    window.__leveeReq = { inflight: 0, last: Date.now() }
    // A finished request is not a painted page: axios resolves on a microtask
    // and Vue re-renders on the next frame, so the request counter alone still
    // lets an assertion read the pre-paint DOM. Tracking the last DOM mutation
    // makes the settle signal "nothing is changing" — which says nothing about
    // whether what it settled on is CORRECT, so it cannot soften any assertion.
    window.__leveeDom = { last: Date.now() }
    new MutationObserver(() => {
      window.__leveeDom.last = Date.now()
    }).observe(document, { subtree: true, childList: true, characterData: true, attributes: true })
    const counted = (u) => String(u).includes('/api')
    const start = () => {
      window.__leveeReq.inflight++
      window.__leveeReq.last = Date.now()
    }
    const end = () => {
      window.__leveeReq.inflight = Math.max(0, window.__leveeReq.inflight - 1)
      window.__leveeReq.last = Date.now()
    }
    const xhrOpen = XMLHttpRequest.prototype.open
    XMLHttpRequest.prototype.open = function () {
      if (counted(arguments[1])) {
        start()
        this.addEventListener('loadend', end)
      }
      return xhrOpen.apply(this, arguments)
    }
    const nativeFetch = window.fetch
    window.fetch = function (u) {
      if (!counted(u)) return nativeFetch.apply(this, arguments)
      start()
      return nativeFetch.apply(this, arguments).finally(end)
    }
  })
}

// settled reports whether the view has mounted AND has no REST call outstanding.
// It deliberately says nothing about WHAT rendered: the assertions below stay
// the judges of correctness while this only removes the race.
function settled(page) {
  return page.evaluate(() => {
    const title = document.querySelector('.levee-page h1, .levee-page h2, .not-found__title')
    const mounted = !!((title && title.textContent) || '').trim()
    const r = window.__leveeReq
    const d = window.__leveeDom
    if (!mounted || !r || !d) return false
    // Both signals must have been quiet: the LATER of the two timestamps is
    // when activity last happened.
    const lastActivity = Math.max(r.last, d.last)
    return r.inflight === 0 && Date.now() - lastActivity > 150
  })
}

// settleDiagnosis explains, in the page's own terms, why settled() said no.
//
// Why this exists: this suite has a documented history of going red on
// windows-latest on a DIFFERENT assertion each time while the ubuntu leg of the
// same commit passed (CHANGELOG: 「浏览器检查假红的根因坐实」). That fix removed
// the sleep from the synchronisation path but left the timeout opaque — a bare
// "settle timeout" says nothing about WHICH of settled()'s three inputs failed,
// and the assertion that follows reports only its own reading, so the DOM that
// produced the timeout is gone by the time anyone reads the log. Two
// investigations have already paid for that opacity. The three inputs map to
// three different causes:
//   mounted=false   the view never rendered (route chunk failed to load, or the
//                   app never booted) — an application fault, not a timing one.
//   inflight>0      a REST call is still outstanding — a hung or retried request.
//   quiet<=150ms    the DOM is still changing — an animation or a poll loop.
// Reporting all three turns "it flaked again" into a fact about the page.
async function settleDiagnosis(page) {
  let d
  try {
    d = await page.evaluate(() => {
      const titleEl = document.querySelector('.levee-page h1, .levee-page h2, .not-found__title')
      const title = ((titleEl && titleEl.textContent) || '').trim()
      const r = window.__leveeReq
      const dom = window.__leveeDom
      const now = Date.now()
      return {
        title,
        selector: titleEl ? titleEl.tagName.toLowerCase() : null,
        inflight: r ? r.inflight : null,
        sinceReq: r && r.last ? now - r.last : -1,
        sinceDom: dom && dom.last ? now - dom.last : -1,
        states: [...document.querySelectorAll('.state__title')].map((e) => (e.textContent || '').trim()),
        rows: document.querySelectorAll('.el-table__row').length,
        url: location.pathname + location.search,
      }
    })
  } catch (err) {
    // Reading the page can fail for its own reason (mid-navigation context
    // destruction). Say that rather than reporting a confident wrong cause.
    return { summary: 'could not read the page at all: ' + String(err).slice(0, 120), errors: [] }
  }
  const cause = !d.title
    ? 'no route title was ever rendered (the view did not mount)'
    : d.inflight
      ? `a REST call never finished (inflight=${d.inflight}, last activity ${d.sinceReq}ms ago)`
      : `the DOM was still changing (last mutation ${d.sinceDom}ms ago, needs >150ms of quiet)`
  const summary = [
    cause,
    `url=${d.url}`,
    `h1=${JSON.stringify(d.title)}${d.selector ? ' (<' + d.selector + '>)' : ''}`,
    `state titles=${JSON.stringify(d.states)}`,
    `rows=${d.rows}`,
  ].join('; ')
  return { summary, errors: page._errors || [] }
}

async function waitForSettled(page, label) {
  const deadline = Date.now() + SETTLE_DEADLINE_MS
  while (Date.now() < deadline) {
    let done = false
    try {
      done = await settled(page)
    } catch {
      // Navigating mid-poll destroys the execution context; keep polling, the
      // deadline is the backstop and the assertion below is the real verdict.
      done = false
    }
    if (done) return true
    await sleep(60)
  }
  const diagnosis = await settleDiagnosis(page)
  notes.push(`settle timeout at ${label} — ${diagnosis.summary}`)
  console.log(`  NOTE settle timeout after ${SETTLE_DEADLINE_MS}ms at ${label} — asserting on what is on screen`)
  console.log(`       why: ${diagnosis.summary}`)
  if (diagnosis.errors.length) console.log(`       page errors: ${JSON.stringify(diagnosis.errors)}`)
  // A screenshot of the state that produced the timeout. Only taken here (and
  // only with --shots) because the failing region is not necessarily one of the
  // routes test [1] screenshots — the empty tier of test [2] is exactly that,
  // which is why the previous two investigations had to reason from assertion
  // text alone.
  if (SHOTS) {
    const shot = `${SHOT_DIR}/settle-timeout${label.replace(/\W+/g, '_')}.png`
    try {
      await page.screenshot({ path: shot })
      console.log(`       screenshot: ${shot}`)
    } catch (err) {
      console.log(`       screenshot failed: ${String(err).slice(0, 120)}`)
    }
  }
  return false
}

// No per-call settle budget: the deadline is a single global knob
// (SETTLE_DEADLINE_MS). The sleep era used to pass a per-call millisecond
// count here, and three call sites still carried the argument long after the
// wait stopped reading it — a number that looked like configuration and had no
// effect. If a page ever genuinely needs longer, that is an argument about the
// deadline for everyone, not a quiet per-site override.
async function open(page, path) {
  // domcontentloaded, never networkidle2: the monitor's SSE stream keeps a
  // connection open, so "idle" never arrives and a networkidle wait just burns
  // the timeout.
  await page.goto(BASE + path, { waitUntil: 'domcontentloaded', timeout: 30_000 })
  // The former `await sleep(settleMs)` here was the suite's only synchronisation
  // between "navigation started" and "assertions read the DOM", and it was
  // wrong: the view's chunk is compiled on demand by Vite and its data arrives
  // asynchronously, so on a contended runner the read happened before the
  // paint. That is not hypothetical — three unrelated dependency bumps each went
  // red on a DIFFERENT assertion (`rows=0`, `title=""`, an unchanged url) with
  // the page reporting no error, on windows only, while the ubuntu leg of the
  // same commit passed. Deleting the sleeps reproduces those exact signatures
  // locally. Waiting for the page's own outstanding-request count removes the
  // clock as a judge without weakening what is asserted.
  return waitForSettled(page, path)
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
  const deadline = Date.now() + Math.max(SETTLE_DEADLINE_MS, 60_000)
  while (Date.now() < deadline) {
    const h1 = await page.evaluate(() => document.querySelector('.levee-page h1')?.textContent?.trim() || '')
    const rows = await page.evaluate(() => document.querySelectorAll('.el-table__row').length)
    if (h1 && rows > 0) break
    await sleep(500)
  }
  await page.close()
}

const text = (page, sel) => page.evaluate((s) => (document.querySelector(s)?.textContent || '').trim(), sel)

// How long Chrome gets to start. Deliberately a SEPARATE budget from
// SETTLE_DEADLINE_MS, and the difference is the whole argument for one being
// raisable and the other not:
//
//   - SETTLE_DEADLINE_MS gates when we stop observing the page and assert on
//     what is there, so raising it would weaken what the assertions mean. It is
//     a judgement proxy and stays where it is.
//   - This one waits for an external process to print its websocket endpoint.
//     No assertion depends on how long that takes, so a longer budget cannot
//     turn a wrong verdict into a right one.
//
// puppeteer's default is 30s. A windows CI runner has exceeded it: run
// 39949099138's sibling job died with "Timed out after 30000 ms while waiting
// for the WS endpoint URL to appear in stdout" — Chrome never started, 38s
// after "launching Chrome" and with no screenshot and no settle note, which is
// a DIFFERENT failure from the settle-timeout flake this suite's notes describe.
// Local measurement: 0.4–1.2s idle. 90s is a cold-filesystem allowance, not a
// contended-machine estimate.
const CHROME_LAUNCH_TIMEOUT_MS = Number(process.env.BROWSER_CHECK_LAUNCH_MS || 90_000)

console.log('browser-check: launching Chrome')
let browser
try {
  browser = await puppeteer.default.launch({
    executablePath: CHROME,
    timeout: CHROME_LAUNCH_TIMEOUT_MS,
    args: ['--no-sandbox', '--headless=new', '--disable-dev-shm-usage', '--hide-scrollbars', '--force-color-profile=srgb'],
  })
} catch (err) {
  // Name which failure this is. The launcher's own message is one line that
  // reads like an assertion failure, and the two have completely different
  // responses: one means the console regressed, the other means the runner was
  // slow to start a browser process.
  console.error(
    `browser-check: Chrome did not start within ${CHROME_LAUNCH_TIMEOUT_MS}ms. ` +
      `This is a runner/browser-startup failure, NOT a console assertion: ${err && err.message ? err.message : err}`,
  )
  process.exit(1)
}

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
  await open(page, path)
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
  await open(page, '/changes')
  const okRows = await page.evaluate(() => document.querySelectorAll('.el-table__row').length)
  check('normal mode shows rows', okRows > 0, `rows=${okRows}`)

  // forbidden: the region answers in place, and the table is not silently empty
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=forbidden')
  await open(page, '/changes')
  const forbiddenText = await text(page, '.state__title')
  check('403 renders the forbidden tier (not "no data")', forbiddenText.includes('没有权限'), `title="${forbiddenText}"`)

  // empty: a genuinely empty list is its own answer
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=empty')
  await open(page, '/changes')
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
  await open(page, '/changes')
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
  await open(page, '/monitor/run-fleet')
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
  const bottom = await page.evaluate(async (deadlineMs) => {
    const read = () => {
      const rows = [...document.querySelectorAll('.map tbody tr')].filter((r) => !r.classList.contains('map__spacer'))
      const spacers = document.querySelectorAll('.map__spacer')
      return {
        rows: rows.length,
        padTop: spacers.length ? Math.round(spacers[0].getBoundingClientRect().height) : -1,
        padBottom: spacers.length > 1 ? Math.round(spacers[1].getBoundingClientRect().height) : -1,
        first: rows.length ? rows[0].querySelector('td').textContent : '',
        last: rows.length ? rows[rows.length - 1].querySelector('td').textContent : '',
      }
    }
    const box = document.querySelector('.map-scroll')
    const before = read().first
    box.scrollTop = box.scrollHeight
    // The re-windowing is a Vue reactive tick, so wait until the first visible
    // host actually CHANGES — a settle signal that is not the value being
    // asserted below — instead of trusting a fixed 300ms.
    const until = Date.now() + deadlineMs
    let cur = read()
    while (Date.now() < until) {
      cur = read()
      if (cur.first !== before) break
      await new Promise((r) => setTimeout(r, 40))
    }
    return cur
  }, SETTLE_DEADLINE_MS)
  check('scrolling to the bottom lands on the last rows', bottom.padBottom === 0, `padBottom=${bottom.padBottom}`)
  check('the window shrank to what is left', bottom.rows > 0 && bottom.rows < 100, `rows=${bottom.rows}`)
  if (SHOTS) await page.screenshot({ path: `${SHOT_DIR}/monitor_fleet.png` })
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[4c] both densities agree with the pitches the windowing math uses')
{
  // Why this is a browser check and not a unit test: MAP_ROW_PITCH{,_COMPACT}
  // are MEASURED constants, so the unit tests can only compare them with
  // themselves. Nothing there can tell whether the stylesheet still produces
  // those pitches — which is exactly the drift that made the map's scrollbar
  // lie the first time (the CSS box model said 21, reality said 27). Measuring
  // the real DOM at BOTH densities is what keeps the two in step.
  const measured = {}
  for (const [density, expectedPitch, expectedCellPad] of [
    ['comfortable', 27, '4px'],
    ['compact', 23, '2px'],
  ]) {
    const page = await newPage(browser)
    // The inline bootstrap in index.html reads this before first paint, so
    // setting it through evaluateOnNewDocument exercises the real path rather
    // than adding the class by hand.
    await page.evaluateOnNewDocument((d) => localStorage.setItem('levee-density', d), density)
    await open(page, '/monitor/run-fleet')
    const m = await page.evaluate(() => {
      const box = document.querySelector('.map-scroll')
      const rows = [...document.querySelectorAll('.map tbody tr')].filter((r) => !r.classList.contains('map__spacer'))
      const padY = getComputedStyle(document.querySelector('.map__cell')).paddingTop
      const pitch = rows.length > 1
        ? Math.round(rows[1].getBoundingClientRect().top - rows[0].getBoundingClientRect().top)
        : 0
      return { pitch, padY, scrollH: Math.round(box.scrollHeight), compact: document.documentElement.classList.contains('density-compact') }
    })
    check(`${density}: the class is ${density === 'compact' ? 'applied' : 'absent'}`,
      m.compact === (density === 'compact'), `compact-class=${m.compact}`)
    check(`${density}: the map cell padding is ${expectedCellPad}`, m.padY === expectedCellPad, `padding-top=${m.padY}`)
    check(`${density}: the row pitch is the measured ${expectedPitch}px`, m.pitch === expectedPitch, `pitch=${m.pitch}`)
    // 300 hosts in the fixture, plus the header row whose own padding follows
    // the same knob (which is why this is a tolerance, not an equality: the
    // header contributes 47px comfortable and 44px compact).
    check(`${density}: the scrollbar still describes every host`,
      Math.abs(m.scrollH - (300 * expectedPitch + 45)) < 6, `scrollHeight=${m.scrollH}`)
    measured[density] = m.pitch
    await page.close()
  }
  check('compact is denser, not merely spelled differently',
    measured.compact < measured.comfortable, `${measured.compact} < ${measured.comfortable}`)

  // Leave the shared browser context back at the default: localStorage is per
  // profile, not per page, so every block after this one would otherwise run
  // compact.
  const reset = await newPage(browser)
  await reset.evaluateOnNewDocument(() => localStorage.setItem('levee-density', 'comfortable'))
  await open(reset, '/changes')
  const restored = await reset.evaluate(() => document.documentElement.classList.contains('density-compact'))
  check('the density block leaves the default behind for the rest of the run', restored === false, `still-compact=${restored}`)
  await reset.close()
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
  await open(page, '/cluster')
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
  await open(page, '/changes')
  await page.keyboard.press('j')
  // Wait until the cursor EXISTS, then assert there is exactly one: waiting on
  // presence keeps "no cursor at all" distinguishable from "two cursors", which
  // a wait on the final count would erase.
  await page.waitForFunction(() => document.querySelectorAll('.lv-row-cursor').length > 0,
    { timeout: SETTLE_DEADLINE_MS, polling: 60 }).catch(() => null)
  const cursor = await page.evaluate(() => document.querySelectorAll('.lv-row-cursor').length)
  check('j paints a row cursor', cursor === 1, `cursors=${cursor}`)
  // The property here is "Enter navigates", so the effect is the measurement;
  // the old 900ms sleep was a budget nobody chose, and on a contended runner the
  // route change is still in flight when it expires. Same verdict, honest budget.
  await page.keyboard.press('Enter')
  await page.waitForFunction(() => location.pathname.includes('/changes/run-'),
    { timeout: SETTLE_DEADLINE_MS, polling: 60 }).catch(() => null)
  const onDetail = page.url().includes('/changes/run-')
  check('Enter opens the cursor row', onDetail, `url=${page.url()}`)
  await page.close()
}

// ---------------------------------------------------------------------------
console.log('\n[6] the day boundary follows the operator, not the server')
{
  const page = await newPage(browser)
  // newPage installs the request counter open() waits on; this second
  // installation wraps it and only records the trend URLs.
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
  await open(page, '/changes')
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
  await open(page, '/cluster')
  const hint = await text(page, '.hint')
  check('sqlite deployment explains that coordination is off',
    hint.indexOf('未启用集群协调') >= 0, 'hint=' + JSON.stringify(hint.slice(0, 60)))
  const cards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('and shows no fake node cards', cards === 0, 'cards=' + cards)

  // Cluster mode (PG backend): one card per registered node, and an offline
  // node coloured bad rather than ok.
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=cluster')
  await open(page, '/cluster')
  const clusterCards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('cluster deployment renders one card per node', clusterCards === 3, 'cards=' + clusterCards)
  const offline = await page.evaluate(() => document.querySelectorAll('.node .lv-dot--bad').length)
  check('an offline node is coloured bad, not ok', offline === 1, 'offline=' + offline)

  // Declared capabilities render on the node card — the cluster_nodes column's
  // first reader. The PascalCase leak this fixes is why the assertion also
  // covers the field values: a node whose id/address render but whose
  // capabilities do not is the half-surfaced state.
  const caps = await page.evaluate(() =>
    [...document.querySelectorAll('.node')].map((card) => ({
      id: card.querySelector('.node__id')?.textContent?.trim() || '',
      caps: [...card.querySelectorAll('.node__cap')].map((c) => c.textContent?.trim() || ''),
      none: !!card.querySelector('.node__caps--none'),
    })),
  )
  const byId = Object.fromEntries(caps.map((c) => [c.id, c]))
  check('a node renders its declared capabilities', JSON.stringify(byId['node-a'].caps) === JSON.stringify(['shell', 'pkg', 'file']), JSON.stringify(caps))
  check('a node that declared none says so', byId['node-c'].none === true, JSON.stringify(caps))

  // Coordination ON with an empty registry. The storage axis cannot express
  // this deployment: postgres + no nodes looked exactly like a deliberate
  // single-node server, so the one sentence an operator needed — "your master
  // never registered" — was the one the wire could not carry.
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=cluster-empty')
  await open(page, '/cluster')
  const emptyHint = await text(page, '.hint')
  check('a cluster with an empty registry is reported as one',
    emptyHint.indexOf('节点注册表为空') >= 0, 'hint=' + JSON.stringify(emptyHint.slice(0, 80)))
  check('and does not borrow the coordination-off sentence',
    emptyHint.indexOf('未启用集群协调') < 0, 'hint=' + JSON.stringify(emptyHint.slice(0, 80)))
  const emptyCards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('an empty registry renders no node cards', emptyCards === 0, 'cards=' + emptyCards)

  // Node cards only exist while coordination is on, so the capability axis has
  // to be exercised in cluster mode — asserting a filter against the sqlite
  // deployment would read `[]` and look like a working filter.
  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=cluster')

  // Capability filtering. The registry has always STORED a per-node capability
  // list and nothing in the product could ask about it, so "do we have any node
  // that can run pkg" was unanswerable while looking answerable. The filter is
  // driven through the URL (the page seeds it from ?capability=, like the changes
  // board's ?q=), which is both shareable and free of synthetic typing.
  await open(page, '/cluster?capability=pkg')
  const pkgCards = await page.evaluate(() =>
    [...document.querySelectorAll('.lv-panel.node .node__id')].map((e) => (e.textContent || '').trim()),
  )
  check('the capability filter narrows to the nodes declaring it',
    JSON.stringify(pkgCards) === JSON.stringify(['node-a']), JSON.stringify(pkgCards))

  // The counts next to a filtered list are NOT filtered: assignments are placed
  // by load, never by capability. A page that left that unstated would imply the
  // scheduler honours declarations. Scoped to the caveat span: the page has
  // several section titles and the first one belongs to the batch panel.
  const caveat = await text(page, '.lv-section-title .lv-panel__hint')
  check('a filtered page says the assignment counts stay deployment-wide',
    caveat.indexOf('全部署口径') >= 0 && caveat.indexOf('不按能力') >= 0, 'caveat=' + JSON.stringify(caveat))

  await open(page, '/cluster?capability=nonexistent')
  const noMatchHint = await text(page, '.hint')
  check('a no-match filter is described as a declaration gap, not an empty registry',
    noMatchHint.indexOf('没有节点声明能力') >= 0 && noMatchHint.indexOf('节点注册表为空') < 0,
    'hint=' + JSON.stringify(noMatchHint.slice(0, 80)))
  const noMatchCards = await page.evaluate(() => document.querySelectorAll('.lv-panel.node').length)
  check('and shows no node cards', noMatchCards === 0, 'cards=' + noMatchCards)

  await page.evaluate((u) => fetch(u, { mode: 'cors' }), MOCK + '/__mode?mode=ok')
  await page.close()
}

console.log(`\nbrowser-check: ${failures.length === 0 ? 'ALL PASS' : failures.length + ' FAILED'}`)
if (failures.length) {
  for (const f of failures) console.log('  - ' + f)
}
// The settle notes are printed here, not only where they happen. They used to
// be collected into an array that nothing ever read — recorded and discarded.
// They matter most exactly when something failed: each one states that an
// assertion downstream of it read the DOM before the page had settled, which is
// the first thing to check before believing a failure is a product regression.
if (notes.length) {
  console.log(`\n${notes.length} settle note(s) — an assertion after one of these read an unsettled page:`)
  for (const n of notes) console.log('  ' + n)
}
await browser.close()
process.exit(failures.length === 0 ? 0 : 1)
