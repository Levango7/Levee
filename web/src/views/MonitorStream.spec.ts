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

describe('monitor event stream', () => {
  it('subscribes to the server stream instead of polling', () => {
    expect(view, 'the stream must be opened against the events route').toContain(
      'new EventSource(streamUrl())',
    )
    expect(view, 'the stream url must target the change events endpoint').toContain(
      '/changes/${changeId.value}/events',
    )
    // The timer this replaces must not be able to come back silently.
    expect(view, 'no polling timer').not.toContain('setInterval')
    expect(view, 'no polling constant').not.toContain('POLL_INTERVAL')
  })

  it('closes the stream when the view goes away', () => {
    // A stream left open on unmount is a subscription the change keeps
    // publishing to after the operator navigated away.
    expect(view, 'unmount must close the stream').toContain('onUnmounted(closeStream)')
    expect(view, 'closeStream must close the EventSource').toContain('eventSource.value.close()')
  })

  it('re-opens when the route points at another change', () => {
    // Opening a run from the change list must not leave the previous
    // change's events flowing into this view.
    expect(view, 'watch(changeId) must re-open the stream').toContain('watch(changeId,')
  })

  it('keeps a disconnect from becoming a spin loop', () => {
    // EventSource does not retry once closed, so a backoff has to live here;
    // an unthrottled reconnect loop would hammer the server on an outage.
    expect(view).toContain('Math.min(retryDelay.value * 2, 30_000)')
  })

  it('reads the aggregates on an event, because the event carries no snapshot', () => {
    // Status and batch panels are snapshots; the event only says "something
    // changed". Reloading them is the honest response.
    expect(view, 'an event must refresh the aggregates').toContain('void loadSummary(true)')
    expect(view, 'an event must refresh the change').toContain('void loadChange(true)')
    // The quiet flag is load-bearing, not a style choice: without it a
    // stream-driven failure would raise a region error (or, before that, a
    // toast) on every event of a flaky network, while the panels above keep
    // their last real numbers. refreshAll (mount / manual 刷新) passes nothing,
    // so a manual refresh still answers in place — see regionStates.spec.ts.
    expect(view, 'the stream path must stay quiet').toContain('void loadLogs(true)')
    expect(view, 'the manual path must be loud').toContain('await Promise.all([loadChange(), loadLogs(), loadSummary()])')
  })
})
