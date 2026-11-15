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
