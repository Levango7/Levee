#!/usr/bin/env node
// mock-wire-check.mjs — the mock gateway's half of the wire contract.
//
// The wire contract already has two guards:
//   internal/grpc/wire_snapshot_test.go  — the SERVER records the key sets it
//                                          really sends into api/__contract__/wire.json;
//   src/api/wireContract.spec.ts         — the CLIENT's DTOs are checked against
//                                          that snapshot, both directions.
//
// Neither one looks at web/scripts/mock-gateway.mjs, the third consumer of the
// same contract — and that is the gap the 2026-11-17 audit kept tripping on: a
// hand-written mock that is "nicer" than the wire vouches for a contract the
// product does not have, and the browser check stays green on a page that would
// be blank against the real server. Two live instances this catches on sight:
//   /system/version  the mock sent {version, commit, builtAt}; the wire sends
//                    {version, gitCommit, buildDate, goVersion};
//   /changes         the mock spelled the creator `creator`; the wire says
//                    `createdBy`, so the applicant column renders blank.
//
// The mock is checked against the SAME snapshot, with one deliberate asymmetry
// forced by protojson (which the server uses WITHOUT EmitDefaults):
//
//   ceiling (no invented keys) — every key the mock sends must be a field the
//     client could know about: the DTO interface, the snapshot's observed keys,
//     a justified SERVER_ONLY entry, or a justified EXTRA_PROTO entry. The field
//     universe is the DTO, NOT the snapshot, because the snapshot is only the
//     keys that happened to be non-zero at capture time — `team`/`environment`/
//     `priority` are real Change fields the snapshot omitted for being empty.
//   floor (no dropped always-present keys) — a key that is BOTH in the snapshot
//     AND required (non-optional) in the DTO is one the server demonstrably
//     sends and the client relies on; the mock must not drop it. Keys the
//     server omits-when-zero are deliberately NOT required here: the mock must
//     not be held to a stricter contract than the real server.
//   forbid (no PascalCase) — the snapshot's per-endpoint forbid list.
//
// Run: node web/scripts/mock-wire-check.mjs   (exit 1 on any drift)

import { spawn } from 'node:child_process'
import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))

function locate(rel) {
  let dir = HERE
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (existsSync(p)) return p
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${HERE}`)
}

const wire = JSON.parse(readFileSync(locate('src/api/__contract__/wire.json'), 'utf8'))
const apiSource = readFileSync(locate('src/api/index.ts'), 'utf8')
const typesSource = readFileSync(locate('src/types/levee.ts'), 'utf8')

/** field name -> optional, read out of an `export interface NAME` declaration. */
function interfaceFields(name, source) {
  const header = new RegExp(`export interface ${name}\\s*\\{`)
  const found = header.exec(source)
  if (!found) return null
  const open = found.index + found[0].length - 1
  let depth = 0
  let close = -1
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++
    else if (source[i] === '}') {
      depth--
      if (depth === 0) { close = i; break }
    }
  }
  if (close < 0) throw new Error(`interface ${name} is not closed`)
  const fields = new Map()
  for (const line of source.slice(open + 1, close).split('\n')) {
    const m = /^ {2}([A-Za-z_][A-Za-z0-9_]*)(\?)?:/.exec(line)
    if (m) fields.set(m[1], m[2] === '?')
  }
  return fields
}

function fieldsOf(name) {
  const fields = interfaceFields(name, apiSource) ?? interfaceFields(name, typesSource)
  if (!fields) throw new Error(`no export interface ${name} in api/index.ts or types/levee.ts`)
  return fields
}

// ---------------------------------------------------------------------------
// The endpoints the mock serves that the contract also covers. `dto` checks the
// top-level object; `row`/`rowDto` check the elements of an envelope's array;
// `objects` checks nested objects one level down. `extraProto` names real proto
// fields the client does not read, each with the reason the mock may send them.
// ---------------------------------------------------------------------------
const ENDPOINTS = [
  { name: 'system-status', url: '/api/v1/system/status', dto: 'SystemStatus' },
  { name: 'system-version', url: '/api/v1/system/version', dto: 'VersionInfo' },
  { name: 'changes', url: '/api/v1/changes', row: 'changes', rowDto: 'Change',
    envelope: ['changes', 'totalSize', 'nextPageToken'] },
  { name: 'change', url: '/api/v1/changes/run-1000', dto: 'Change',
    // plan_hash is proto Change field 9 and runToPB sets it; the detail page
    // reads the chain through the evidence panel, not this scalar.
    extraProto: { planHash: 'proto Change.plan_hash; the evidence panel, not this field, is what the page reads' } },
  { name: 'cluster-status', url: '/api/v1/system/cluster-status', dto: 'ClusterStatus',
    row: 'nodes', rowDto: 'ClusterNodeDTO', objects: { summary: 'AssignmentSummaryDTO' } },
  { name: 'change-trend', url: '/api/v1/system/change-trend', dto: 'TrendDTO',
    row: 'buckets', rowDto: 'TrendBucketDTO' },
  { name: 'batch-status', url: '/api/v1/system/batch-status?run_id=run-fleet', dto: 'BatchSummaryDTO',
    row: 'batches', rowDto: 'BatchProgressDTO' },
  { name: 'audit-log', url: '/api/v1/audit/log', row: 'entries',
    envelope: ['entries', 'totalSize', 'nextPageToken'] },
  { name: 'conversation-sessions', url: '/api/v1/conversation/sessions?user_id=dev-user',
    row: 'sessions', rowDto: 'ConversationSessionDTO', envelope: ['sessions'] },
]

// Contract entries the mock does NOT serve at all. Reported as coverage gaps,
// not failures: an unserved endpoint is a place the browser check cannot see,
// which is worth naming without pretending the mock covers it.
const UNCOVERED = ['dbchange-ticket', 'change-with-plan', 'audit-traces', 'agents', 'groups']
// Served by the mock but not key-checked here: takeover-events has no DTO in
// the contract's read set, and rollback is a POST whose preview/commit pair the
// browser check already drives directly. Named so the coverage story is honest.
const SERVED_NOT_KEYED = ['takeover-events', 'rollback']

const results = []
function record(ok, label, detail) {
  results.push({ ok, label, detail })
  console.log(`  ${ok ? 'ok  ' : 'FAIL'} ${label}${detail ? ' — ' + detail : ''}`)
}

// Check one object's keys against its contract level.
function checkObject(endpoint, path, actual, dtoName, wireKeys, serverOnly, extraProto) {
  const dto = dtoName ? fieldsOf(dtoName) : new Map()
  const universe = new Set([...dto.keys(), ...wireKeys, ...serverOnly, ...Object.keys(extraProto)])
  const actualKeys = Object.keys(actual)
  const invented = actualKeys.filter((k) => !universe.has(k))
  record(invented.length === 0, `${endpoint}${path}: no invented keys`,
    invented.length ? `mock sends ${JSON.stringify(invented)} which no DTO/wire/proto field declares` : `${actualKeys.length} keys all declared`)
  // floor: keys the server sent in the snapshot AND the DTO requires.
  const required = new Set([...dto.entries()].filter(([, opt]) => !opt).map(([k]) => k))
  const floor = wireKeys.filter((k) => required.has(k))
  const missing = floor.filter((k) => !(k in actual))
  record(missing.length === 0, `${endpoint}${path}: always-present keys present`,
    missing.length ? `mock drops ${JSON.stringify(missing)} (on the wire AND required by ${dtoName})` : `floor=${JSON.stringify(floor)}`)
  // PascalCase leaks.
  const forbid = wire[endpoint]?.forbid ?? []
  const leaked = actualKeys.filter((k) => forbid.includes(k))
  record(leaked.length === 0, `${endpoint}${path}: no PascalCase leak`,
    leaked.length ? JSON.stringify(leaked) : '')
}

async function fetchJson(port, url) {
  const res = await fetch(`http://127.0.0.1:${port}${url}`)
  if (res.status !== 200) return { __status: res.status, body: await res.text() }
  return { __status: 200, body: await res.json() }
}

async function main() {
  const port = 4600 + (process.pid % 200)
  const child = spawn(process.execPath, [join(HERE, 'mock-gateway.mjs')], {
    env: { ...process.env, MOCK_PORT: String(port) },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let ready = false
  child.stdout.on('data', (d) => { if (String(d).includes('listening')) ready = true })
  child.stderr.on('data', (d) => process.stderr.write(d))
  const deadline = Date.now() + 10000
  while (!ready && Date.now() < deadline) await new Promise((r) => setTimeout(r, 100))
  if (!ready) { child.kill(); throw new Error('mock gateway did not start') }

  try {
    for (const ep of ENDPOINTS) {
      const w = wire[ep.name]
      if (!w) { record(false, ep.name, 'no wire.json entry — cannot check'); continue }
      const { __status, body } = await fetchJson(port, ep.url)
      if (__status !== 200) { record(false, ep.name, `mock answered HTTP ${__status}: ${JSON.stringify(body).slice(0, 80)}`); continue }
      const serverOnly = []
      const extra = ep.extraProto ?? {}
      if (ep.dto) {
        checkObject(ep.name, '', body, ep.dto, w.keys ?? [], serverOnly, extra)
      } else if (ep.envelope) {
        // Envelope: only the ceiling applies (the envelope has no DTO); the
        // server's own envelope keys plus the pagination fields are the universe.
        const keys = Object.keys(body)
        const universe = new Set([...(w.keys ?? []), ...ep.envelope])
        const invented = keys.filter((k) => !universe.has(k))
        record(invented.length === 0, `${ep.name}: envelope keys`,
          invented.length ? `invented ${JSON.stringify(invented)}` : JSON.stringify(keys))
        const missing = (w.keys ?? []).filter((k) => !(k in body))
        record(missing.length === 0, `${ep.name}: envelope carries the server's keys`,
          missing.length ? `drops ${JSON.stringify(missing)}` : '')
      }
      if (ep.row && Array.isArray(body[ep.row]) && body[ep.row].length > 0) {
        const rowWire = w.rows?.[ep.row] ?? []
        body[ep.row].forEach((r, i) =>
          checkObject(ep.name, `.${ep.row}[${i}]`, r, ep.rowDto, rowWire, [], {}))
      }
      for (const [objPath, objDto] of Object.entries(ep.objects ?? {})) {
        const val = body[objPath]
        if (val && typeof val === 'object' && !Array.isArray(val)) {
          checkObject(ep.name, `.${objPath}`, val, objDto, w.objects?.[objPath] ?? [], [], {})
        }
      }
    }
  } finally {
    child.kill()
  }

  console.log('\ncoverage gaps (in the contract, not served by the mock — the browser check cannot see these):')
  for (const name of UNCOVERED) console.log(`  - ${name} (${wire[name]?.method ?? '?'} ${wire[name]?.path ?? '?'})`)
  console.log('served but not key-checked here (no DTO read-set / POST driven by the browser check):')
  for (const name of SERVED_NOT_KEYED) console.log(`  - ${name}`)

  const failed = results.filter((r) => !r.ok)
  console.log(`\n${results.length - failed.length}/${results.length} mock-wire checks passed`)
  if (failed.length) {
    console.log('drift:')
    for (const f of failed) console.log('  - ' + f.label + (f.detail ? ': ' + f.detail : ''))
    process.exit(1)
  }
}

main().catch((err) => { console.error(err); process.exit(1) })
