// useSSEStream.spec.ts — the live-feed client's contract.
//
// The three properties that matter, in the order they were broken:
//
//   1. the request carries the bearer token the console holds — EventSource
//      could not send one, so every token-protected deployment answered 401
//      and the console retried forever in silence;
//   2. a NAMED frame reaches the handler under its name — the server writes
//      "event: status_changed", and a listener limited to the default type
//      hears nothing;
//   3. a 401 ends the stream instead of reconnecting with the same refused
//      credential — and the console's shared 401 policy takes over.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { setToken } from '@/api/client'
import { createSSEStream, type SSEStatus } from './useSSEStream'

interface StreamCall {
  url: string
  init: RequestInit | undefined
}

/** A fetch stub that answers with the given responses in order. */
function stubFetch(responses: Array<{ status: number; body?: string }>): { calls: StreamCall[] } {
  const calls: StreamCall[] = []
  let i = 0
  const stub = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init })
    const spec = responses[Math.min(i, responses.length - 1)]
    i++
    const body = spec?.body ?? ''
    const encoder = new TextEncoder()
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        if (body) controller.enqueue(encoder.encode(body))
        controller.close()
      },
    })
    return {
      ok: (spec?.status ?? 200) >= 200 && (spec?.status ?? 200) < 300,
      status: spec?.status ?? 200,
      body: stream,
    } as unknown as Response
  })
  vi.stubGlobal('fetch', stub)
  return { calls }
}

function waitFor(predicate: () => boolean, timeoutMs = 2_000): Promise<void> {
  return new Promise((resolve, reject) => {
    const started = Date.now()
    const tick = (): void => {
      if (predicate()) {
        resolve()
        return
      }
      if (Date.now() - started > timeoutMs) {
        reject(new Error('timed out waiting for the condition'))
        return
      }
      setTimeout(tick, 5)
    }
    tick()
  })
}

describe('useSSEStream', () => {
  beforeEach(() => {
    setToken('token-abc')
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    setToken('')
  })

  it('sends the bearer token the console holds', async () => {
    const { calls } = stubFetch([{ status: 200, body: 'data: one\n\n' }])
    const stream = createSSEStream({ url: '/api/v1/alerts/stream', onEvent: () => {} })
    stream.connect()
    await waitFor(() => calls.length > 0)
    const headers = (calls[0]?.init?.headers ?? {}) as Record<string, string>
    expect(headers.Authorization).toBe('Bearer token-abc')
    stream.close()
  })

  it('delivers a named frame under its name', async () => {
    stubFetch([{ status: 200, body: 'event: status_changed\ndata: {"changeId":"run-1"}\n\n' }])
    const seen: string[] = []
    const stream = createSSEStream({
      url: '/api/v1/changes/run-1/events',
      onEvent: (ev) => seen.push(ev.event),
    })
    stream.connect()
    await waitFor(() => seen.length > 0)
    expect(seen[0]).toBe('status_changed')
    stream.close()
  })

  it('stops on 401 instead of reconnecting with the refused credential', async () => {
    const { calls } = stubFetch([{ status: 401 }])
    const states: SSEStatus[] = []
    const stream = createSSEStream({
      url: '/api/v1/alerts/stream',
      onEvent: () => {},
      onStatus: (s) => states.push(s),
      retryDelayMs: 10,
    })
    stream.connect()
    await waitFor(() => states.includes('unauthorized'))
    // Give the loop a chance to misbehave: no second attempt, no reconnect.
    await new Promise((r) => setTimeout(r, 60))
    expect(calls).toHaveLength(1)
    expect(states.filter((s) => s === 'unauthorized')).toHaveLength(1)
    stream.close()
  })

  it('reconnects after the stream ends, with a bounded backoff', async () => {
    const { calls } = stubFetch([{ status: 200, body: 'data: one\n\n' }])
    const stream = createSSEStream({
      url: '/api/v1/alerts/stream',
      onEvent: () => {},
      retryDelayMs: 10,
      maxRetryDelayMs: 20,
    })
    stream.connect()
    await waitFor(() => calls.length >= 2)
    expect(calls.length).toBeGreaterThanOrEqual(2)
    stream.close()
  })

  it('stops reconnecting after close()', async () => {
    const { calls } = stubFetch([{ status: 200, body: 'data: one\n\n' }])
    const stream = createSSEStream({
      url: '/api/v1/alerts/stream',
      onEvent: () => {},
      retryDelayMs: 10,
    })
    stream.connect()
    await waitFor(() => calls.length >= 1)
    stream.close()
    const seen = calls.length
    await new Promise((r) => setTimeout(r, 80))
    expect(calls).toHaveLength(seen)
    expect(stream.status.value).toBe('closed')
  })
})
