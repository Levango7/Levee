// Unit tests for the DB Change Ticket export client.
//
// The export is a second READING of the same ticket, not a second renderer:
// the browser asks the server for ?format=markdown and saves what comes back.
// Two things are therefore load-bearing, and both are asserted here:
//
//  1. the request carries the format parameter (without it the server answers
//     protojson, and the "document" would be a JSON blob with a .md name);
//  2. the body arrives as TEXT, byte for byte — a client that JSON-parsed it
//     would hand the file a re-serialised object, and a document that differs
//     from the server's rendering is a document nobody can trust.
import { describe, expect, it, vi } from 'vitest'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

const DOCUMENT = '# 数据库变更发布单\n\n- 变更 ID: `run-1`\n'

describe('changesApi.ticketDocument', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  it('asks the same endpoint for the markdown form and returns the text', async () => {
    vi.resetModules()
    const mod = await import('./index')
    const client = await import('./client')

    const seen: { url?: string; method?: string; responseType?: unknown } = {}
    client.axiosClient.defaults.adapter = async (config) => {
      seen.url = config.url
      seen.method = config.method
      seen.responseType = config.responseType
      return { data: DOCUMENT, status: 200, statusText: 'OK', headers: {}, config }
    }

    const doc = await mod.changesApi.ticketDocument('run-1')
    expect(seen.url).toBe('/changes/run-1/ticket?format=markdown')
    expect(seen.method).toBe('get')
    expect(seen.responseType).toBe('text')
    expect(doc).toBe(DOCUMENT)
  })

  it('escapes the change id in the path', async () => {
    // The id reaches the URL; an unescaped one would let a crafted id pick a
    // different route (path traversal in a URL is still a URL).
    vi.resetModules()
    const mod = await import('./index')
    const client = await import('./client')

    const seen: { url?: string } = {}
    client.axiosClient.defaults.adapter = async (config) => {
      seen.url = config.url
      return { data: DOCUMENT, status: 200, statusText: 'OK', headers: {}, config }
    }

    await mod.changesApi.ticketDocument('a/b c')
    expect(seen.url).toBe('/changes/a%2Fb%20c/ticket?format=markdown')
  })
})

// ---------------------------------------------------------------------------
// The white-screen regression: the client must NOT unwrap the response.
//
// `GET /changes/:id/ticket` answers with the DBChangeTicket FLAT at the top
// level (`writeProto` of the pb message — `internal/grpc/rest.go`
// handleChangeTicket). The wire contract records that shape as the
// `dbchange-ticket` entry in api/__contract__/wire.json: its keys are
// `change`, `sql`, `rollback`, `window`, `approval`, `execution`, `audit`.
//
// The console used to call `.then((r) => r.ticket)` — a shape the server never
// sends. The request SUCCEEDED, the promise resolved to `undefined`, and the
// page rendered nothing: a blank sheet with no error anywhere. Every existing
// guard missed it because they were structural (they read this file's source,
// they never drove it).
//
// This test drives it: mock the transport with the server's real shape, and
// assert the caller receives a ticket whose sections are reachable. An unwrap
// makes this fail immediately.
describe('changesApi.ticket returns the flat contract', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  // The server's actual shape, as recorded in the committed wire snapshot.
  const FLAT = {
    change: { id: 'run-1', label: '账户表迁移', status: 'completed', planState: 'readable' },
    sql: { hasSql: true, steps: [{ step: 'migrate', sql: 'UPDATE t SET a=1' }] },
    rollback: { strategy: 'snapshot', hasUndo: true },
    window: { enforcedAt: 'plan', persistedPerRun: false },
    approval: { hasApproval: true, level: 'standard', status: 'approved', matchesPlan: true },
    execution: { verdict: 'complete', hasEvidence: true },
    audit: { valid: true, verified: true, count: 3 },
  }

  it('resolves to the ticket itself, with every section reachable', async () => {
    vi.resetModules()
    const mod = await import('./index')
    const client = await import('./client')
    const seen: { url?: string } = {}
    client.axiosClient.defaults.adapter = async (config) => {
      seen.url = config.url
      return { data: FLAT, status: 200, statusText: 'OK', headers: {}, config }
    }

    const ticket = await mod.changesApi.ticket('run-1')

    expect(seen.url).toBe('/changes/run-1/ticket')
    // The regression: an unwrap makes every one of these `undefined`.
    expect(ticket).toBeDefined()
    expect(ticket.change?.id).toBe('run-1')
    expect(ticket.change?.planState).toBe('readable')
    expect(ticket.sql?.hasSql).toBe(true)
    expect(ticket.rollback?.strategy).toBe('snapshot')
    expect(ticket.window?.enforcedAt).toBe('plan')
    expect(ticket.approval?.matchesPlan).toBe(true)
    expect(ticket.execution?.verdict).toBe('complete')
    expect(ticket.audit?.valid).toBe(true)
  })
})
