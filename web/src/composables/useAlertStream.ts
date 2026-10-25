// useAlertStream — the console's one live alert connection.
//
// The stream is /alerts/stream (SSE): the backend replays the ring snapshot
// then tails live, so a subscriber that opens the connection receives
// everything currently known plus everything that fires afterwards. The
// connection's lifecycle follows the ROUTE (open on the alerts page, closed on
// leave) rather than being global — an always-on socket in a shell component
// would spend a server-side subscriber for every open tab whether anyone is
// looking at alerts or not. The badge composable reuses this module's count so
// the header shows what this stream saw without opening a second connection.
//
// Visibility is server-side: the backend narrows by the alert's OWNED
// environment before framing, so this composable renders what arrives and
// never re-filters.

import { computed, readonly, ref, onBeforeUnmount, type Ref, type ComputedRef } from 'vue'

import type { AlertRecord } from '@/api'
import { ALERT_SEVERITIES, type AlertSeverity } from '@/types/vocabulary'

const ALERTS_STREAM_URL = '/api/v1/alerts/stream'

const alerts = ref<AlertRecord[]>([])
const connected = ref(false)
// null while the stream is healthy; a string when it errored (the page shows
// the reconnection note instead of pretending the list is live).
const streamError = ref<string | null>(null)

let source: EventSource | null = null
let opened = false

function severityOf(a: AlertRecord): AlertSeverity {
  // The SSE frame carries the numeric enum (json.Marshal of alert.Alert);
  // the proto-JSON status carries the string spelling. Both resolve through
  // the same owner-generated vocabulary.
  const raw =
    typeof a.severity === 'number'
      ? (ALERT_SEVERITIES[a.severity] ?? 'info')
      : String(a.severity)
  return (ALERT_SEVERITIES as readonly string[]).includes(raw)
    ? (raw as AlertSeverity)
    : 'info'
}

function upsert(a: AlertRecord): void {
  const i = alerts.value.findIndex((x) => x.id === a.id)
  if (i >= 0) {
    alerts.value.splice(i, 1, a)
  } else {
    alerts.value.unshift(a)
  }
}

function connect(): void {
  if (source !== null) return
  source = new EventSource(ALERTS_STREAM_URL)
  source.onopen = () => {
    connected.value = true
    streamError.value = null
  }
  source.onmessage = (ev) => {
    try {
      upsert(JSON.parse(ev.data) as AlertRecord)
    } catch {
      // A malformed frame is dropped, not fatal: the next alert re-syncs the
      // list, and killing the stream over one bad frame would blind the page.
    }
  }
  source.onerror = () => {
    // EventSource auto-reconnects; we only record the state so the page can
    // say "重连中" instead of claiming a live list.
    connected.value = false
    streamError.value = '连接断开，正在重连…'
  }
}

function disconnect(): void {
  source?.close()
  source = null
  connected.value = false
}

/** useAlertStream opens the shared SSE connection while the caller is mounted
 *  and closes it on unmount. The alerts array is shared module state, so the
 *  badge and the page see the same list without a second connection. */
export function useAlertStream(): {
  alerts: Readonly<Ref<readonly AlertRecord[]>>
  connected: Readonly<Ref<boolean>>
  streamError: Readonly<Ref<string | null>>
  criticalCount: ComputedRef<number>
} {
  if (!opened) {
    opened = true
    connect()
  }
  // Firing = status 0 (alert.StatusFiring). Missing status on a hand-made
  // frame counts as firing: an alert with no resolution recorded IS firing.

  onBeforeUnmount(() => {
    // Only close when the LAST consumer leaves. The badge lives in the shell
    // (always mounted) and the page mounts/unmounts on navigation, so the
    // refcount is what keeps the header badge alive across navigations.
    consumers--
    if (consumers === 0) {
      opened = false
      disconnect()
    }
  })
  consumers++
  return {
    alerts: readonly(alerts),
    connected: readonly(connected),
    streamError: readonly(streamError),
    criticalCount,
  }
}

// Active critical alerts (firing, severity critical) — the header badge's
// number. Module-level computed so the badge and the page read one source.
const criticalCount = computed(
  () =>
    alerts.value.filter((a) => {
      const firing = a.status === undefined || a.status === 0 || a.status === '0'
      return firing && severityOf(a) === 'critical'
    }).length,
)

// Refcount for the shared connection (see useAlertStream).
let consumers = 0
