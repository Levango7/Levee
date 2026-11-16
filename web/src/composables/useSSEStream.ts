// useSSEStream — the console's live-feed client.
//
// Why this exists instead of the browser's EventSource: EventSource cannot
// send an Authorization header. The gateway's authMiddleware
// (internal/grpc/rest.go) requires one, so in every deployment with a token
// configured the two live feeds answered 401 — and an EventSource failure is
// a NETWORK event, never a page error: the console showed "重连中" forever and
// nothing else. fetch() carries the same bearer token the rest of the
// console uses (web/src/api/client.ts keeps it in localStorage), so the
// stream joins the existing credential path instead of inventing a second
// one.
//
// What the browser gave us for free and this has to do itself:
//
//   - RE-CONNECTION: EventSource retries a dropped connection on its own;
//     fetch does not, so the loop below owns the backoff (doubling, capped).
//   - FRAME PARSING: delegated to SSEParser (sseParser.ts), a pure module
//     with its own unit tests.
//   - REAL STATUS CODES: EventSource hides them; fetch exposes them, so a
//     401 can end the loop and hand the session to the login page instead of
//     hammering the server with a token that was already refused.
//
// What it deliberately does NOT do: no query-string token (it would land in
// browser history and proxy access logs) and no cookie (the server has no
// cookie session layer at all, and inventing one is a different decision with
// its own CSRF obligations).

import { ref, type Ref } from 'vue'

import { getToken, handleUnauthorizedResponse } from '@/api/client'
import { SSEParser, type SSEEvent } from './sseParser'

export type SSEStatus =
  | 'idle'
  | 'connecting'
  | 'open'
  | 'reconnecting'
  | 'closed'
  | 'unauthorized'
  | 'error'

export interface SSEStreamOptions {
  /** The stream URL, or a function returning it (the monitor's URL carries the change id). */
  url: string | (() => string)
  /** Called for every frame that arrives. */
  onEvent: (event: SSEEvent) => void
  /** Called on every state change; the second argument carries a detail string when there is one. */
  onStatus?: (status: SSEStatus, detail?: string) => void
  /** The first reconnect delay, in ms. Doubles on every failed attempt. */
  retryDelayMs?: number
  /** The reconnect delay ceiling, in ms. */
  maxRetryDelayMs?: number
}

export interface SSEStreamHandle {
  /** Open the stream (and keep it open across drops). */
  connect(): void
  /** Close the stream for good: no reconnect, no pending timer. */
  close(): void
  /** The current state of the stream. */
  status: Ref<SSEStatus>
  /** The last "id:" the stream carried, for callers that resume a stream. */
  lastEventId: Ref<string>
}

const DEFAULT_RETRY_DELAY_MS = 1_000
const DEFAULT_MAX_RETRY_DELAY_MS = 30_000

export function createSSEStream(options: SSEStreamOptions): SSEStreamHandle {
  const status = ref<SSEStatus>('idle')
  const lastEventId = ref('')

  let running = false
  let generation = 0
  let controller: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | null = null
  let wake: (() => void) | null = null
  let delay = options.retryDelayMs ?? DEFAULT_RETRY_DELAY_MS
  const maxDelay = options.maxRetryDelayMs ?? DEFAULT_MAX_RETRY_DELAY_MS

  function setStatus(next: SSEStatus, detail?: string): void {
    status.value = next
    options.onStatus?.(next, detail)
  }

  /** sleep waits, and can be cut short by close(). */
  function sleep(ms: number): Promise<void> {
    return new Promise((resolve) => {
      timer = setTimeout(() => {
        timer = null
        wake = null
        resolve()
      }, ms)
      wake = () => {
        if (timer !== null) {
          clearTimeout(timer)
          timer = null
        }
        wake = null
        resolve()
      }
    })
  }

  function deliver(events: SSEEvent[]): void {
    for (const ev of events) {
      if (ev.id) lastEventId.value = ev.id
      options.onEvent(ev)
    }
  }

  async function loop(gen: number): Promise<void> {
    while (running && gen === generation) {
      controller = new AbortController()
      try {
        setStatus(status.value === 'idle' ? 'connecting' : 'reconnecting')
        const url = typeof options.url === 'function' ? options.url() : options.url
        const token = getToken()
        const res = await fetch(url, {
          method: 'GET',
          headers: {
            Accept: 'text/event-stream',
            // The bearer token the console already holds. Re-read on every
            // attempt: a token refreshed by the login page must be picked up
            // by the next reconnect.
            ...(token ? { Authorization: `Bearer ${token}` } : {}),
          },
          cache: 'no-store',
          signal: controller.signal,
        })

        if (res.status === 401 || res.status === 403) {
          // The credential is wrong or expired. Reconnecting would hammer the
          // server with the same refused token; the console's other calls
          // already treat a 401 as "the session ended" (clear + login), so
          // the stream does the same and stops.
          setStatus('unauthorized', `HTTP ${res.status}`)
          if (res.status === 401) handleUnauthorizedResponse()
          running = false
          return
        }
        if (!res.ok || !res.body) {
          throw new Error(`stream refused: HTTP ${res.status}`)
        }

        setStatus('open')
        delay = options.retryDelayMs ?? DEFAULT_RETRY_DELAY_MS

        const parser = new SSEParser()
        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          deliver(parser.push(decoder.decode(value, { stream: true })))
        }
        deliver(parser.push(decoder.decode()))
        deliver(parser.flush())
      } catch (err) {
        // A closed stream is a close(), not a failure — and a loop from an
        // earlier generation must not report on the current one.
        if (!running || gen !== generation) return
        const detail = err instanceof Error ? err.message : String(err)
        // An aborted stream is a close(), not a failure.
        if (detail.includes('abort')) return
        setStatus('error', detail)
      }
      if (!running || gen !== generation) return
      // The stream ended without an error (the server closed it, a proxy
      // timed out, the network dropped): wait, then reconnect. The delay
      // doubles per attempt so a long outage is not hammered.
      setStatus('reconnecting')
      const wait = delay
      delay = Math.min(delay * 2, maxDelay)
      await sleep(wait)
    }
  }

  return {
    status,
    lastEventId,
    connect(): void {
      if (running) return
      running = true
      // A generation counter retires the previous loop: a stream that was
      // still awaiting a read when close() ran must not wake up later and
      // open a second connection.
      const gen = ++generation
      void loop(gen)
    },
    close(): void {
      running = false
      generation++
      controller?.abort()
      controller = null
      wake?.()
      wake = null
      setStatus('closed')
    },
  }
}
