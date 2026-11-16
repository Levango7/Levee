import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// Structural pin for the monitor's event stream. The page polled
// /changes/<id>/logs every 2 seconds for a long time: progress arrived up to
// 2s late, and every poll re-sent the whole tail. The server now streams
// events at /changes/<id>/events (internal/grpc/sse.go), and this pins the
// browser side to it: a timer must not come back, the stream must close on
// unmount (otherwise a navigated-away view holds a subscription open), and it
// must re-open when the route points at another change.
//
// Reads the source rather than a fixture, so it cannot pass on a mock, and
// not finding the file is a hard failure.
//
// The client is fetch-based, not EventSource, and that is pinned here too:
// EventSource cannot send an Authorization header, so in a deployment with a
// token configured the stream answered 401 forever — silently, because an
// EventSource failure is a network event and never a page error. The frames
// the server writes are NAMED ("event: status_changed"), so the handler must
// not be limited to the default "message" type either.

function locate(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (existsSync(p)) return readFileSync(p, 'utf8')
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

const view = locate('src/views/MonitorView.vue')
const client = locate('src/composables/useSSEStream.ts')

describe('monitor event stream', () => {
  it('subscribes to the server stream instead of polling', () => {
    expect(view, 'the stream must be opened against the events route').toContain(
      '/changes/${changeId.value}/events',
    )
    expect(view, 'the stream must go through the shared SSE client').toContain(
      'createSSEStream(',
    )
    // The timer this replaces must not be able to come back silently.
    expect(view, 'no polling timer').not.toContain('setInterval')
    expect(view, 'no polling constant').not.toContain('POLL_INTERVAL')
    // The browser's EventSource cannot carry the bearer token the gateway
    // requires; a client that goes back to it would break every token-
    // protected deployment.
    expect(view, 'the stream must not be read with EventSource').not.toContain('new EventSource')
    expect(client, 'the stream request must carry the bearer token').toContain(
      "Authorization: `Bearer ${token}`",
    )
  })

  it('closes the stream when the view goes away', () => {
    // A stream left open on unmount is a subscription the change keeps
    // publishing to after the operator navigated away.
    expect(view, 'unmount must close the stream').toContain('onUnmounted(closeStream)')
    expect(view, 'closeStream must close the stream').toContain('stream.close()')
  })

  it('re-opens when the route points at another change', () => {
    // Opening a run from the change list must not leave the previous
    // change's events flowing into this view.
    expect(view, 'watch(changeId) must re-open the stream').toContain('watch(changeId,')
  })

  it('keeps a disconnect from becoming a spin loop', () => {
    // fetch() does not retry on its own (EventSource used to), so the client
    // owns the backoff; an unthrottled reconnect loop would hammer the server
    // on an outage.
    expect(client).toContain('Math.min(delay * 2, maxDelay)')
    expect(client).toContain('DEFAULT_MAX_RETRY_DELAY_MS = 30_000')
  })

  it('stops on a refused credential instead of retrying it forever', () => {
    // A 401 must end the loop and hand the session to the login page; the
    // console's other calls already treat a 401 that way.
    expect(client).toContain('res.status === 401 || res.status === 403')
    expect(client).toContain('handleUnauthorizedResponse()')
  })

  it('reads the aggregates on an event, because the event carries no snapshot', () => {
    // Status and batch panels are snapshots; the event only says "something
    // changed". Reloading them is the honest response.
    expect(view, 'an event must refresh the aggregates').toContain('void loadSummary(true)')
    expect(view, 'an event must refresh the change').toContain('void loadChange(true)')
    // The quiet flag is load-bearing, not a style choice: without it, a
    // stream-driven failure would raise a region error (or, before that, a
    // toast) on every event of a flaky network, while the panels above keep
    // their last real numbers. refreshAll (mount / manual 刷新) passes nothing,
    // so a manual refresh still answers in place — see regionStates.spec.ts.
    expect(view, 'the stream path must stay quiet').toContain('void loadLogs(true)')
    expect(view, 'the manual path must be loud').toContain('await Promise.all([loadChange(), loadLogs(), loadSummary()])')
  })
})
