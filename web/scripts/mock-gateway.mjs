// Mock of the LEVEE REST gateway, so the console can be rendered — and tested —
// without a backend.
//
// It is committed on purpose. The console's behaviour lives in how it reacts to
// payloads (a 403 that must not render as "no data", a truncated trend that must
// say so, a day boundary that must follow the operator's clock), and none of
// that is observable from the unit tests alone: they run without a real
// response body. Serving the shapes this file returns turns the browser check
// (browser-check.mjs) into assertions about behaviour rather than a screenshot.
//
// Everything here is a fixture, not a stub of the server: no logic is
// reimplemented, the payloads are just the ones the views consume. Keep them in
// sync with web/src/api/index.ts — the browser check fails loudly when a shape
// drifts, which is the point.
import http from 'node:http'

const now = Math.floor(Date.now() / 1000)

// ---------------------------------------------------------------- fixtures --
const statuses = [
  'running', 'pending', 'failed', 'completed', 'rolled_back_partial', 'approved',
  'paused', 'interrupted', 'draft', 'archived',
]
const labels = [
  '支付网关配置同步', '核心交换机 VLAN 调整', 'K8s 节点池扩容', '数据库主从切换演练',
  '防火墙策略批量下发', '存储卷扩容', '中间件版本升级', 'DNS 记录清理',
  '监控探针更新', '日志采集器重启',
]
const templates = ['gateway-sync', 'k8s-node-drain', 'db-failover-drill']
const changes = labels.map((l, i) => ({
  id: 'run-' + (1000 + i),
  label: l,
  status: statuses[i % statuses.length],
  environment: ['prod', 'staging', 'dev'][i % 3],
  team: '平台组',
  templateName: templates[i % templates.length],
  priority: ['high', 'normal', 'urgent', 'low'][i % 4],
  creator: ['liwg', 'ops-bot', 'zhangs'][i % 3],
  createdAt: now - 3600 * (i + 1),
  updatedAt: now - 600 * i,
}))

const detail = {
  id: 'run-1000', label: labels[0], status: 'running', environment: 'prod', team: '平台组',
  templateName: templates[0], priority: 'high', creator: 'liwg',
  createdAt: now - 3600, updatedAt: now - 300, planHash: 'v2:9f3c1ab7de44',
  params: { region: 'cn-east-1', batch_size: '12' },
}

const trace = {
  entries: [
    { id: 't1', runId: 'run-1000', action: 'plan', actor: 'liwg', target: 'host:10.0.1.11', timestamp: now - 3600, durationMs: 120, input: '{"template":"gateway-sync"}', output: '{"batches":5}', prevHash: '0000', currHash: 'a1b2' },
    { id: 't2', runId: 'run-1000', action: 'approve', actor: 'zhangs', target: '', timestamp: now - 3000, durationMs: 0, input: '', output: '', prevHash: 'a1b2', currHash: 'c3d4' },
    { id: 't3', runId: 'run-1000', action: 'apply', actor: 'ops-bot', target: 'host:10.0.1.11', timestamp: now - 2400, durationMs: 8400, input: '{"batch":2}', output: '{"ok":11,"failed":1}', detail: 'host 10.0.1.14: connection refused', prevHash: 'c3d4', currHash: 'e5f6' },
  ],
  hashChainValid: true, verificationMessage: '链完整',
}

const approvals = [{
  id: 'ap1', level: 'standard', minApprovers: 2, approvers: ['liwg', 'zhangs'],
  decisions: [{ approver: 'liwg', action: 'approve', comment: '窗口内执行' }],
  pending: ['zhangs'], initiator: 'wangw', excludeInitiator: true,
  planHash: 'v2:9f3c1ab7de44', legacy: false, expiresAt: now + 7200,
}]

// The trend: 14 days with a mix of outcomes, including two empty days. The
// empty days are there so the browser check can assert a GAP renders as a gap.
const trendShape = [
  { success: 4, active: 1 }, { success: 2, failed: 1, rolled_back: 1 }, {},
  { success: 6, active: 2 }, { success: 3, rolled_back: 2 }, { success: 5 },
  { success: 4, failed: 2 }, {}, { success: 7, active: 1 }, { success: 3, failed: 1 },
  { success: 6, rolled_back: 1 }, { success: 5, active: 3 }, { success: 8 },
  { success: 4, failed: 1, stopped: 1 },
]

const logs = {
  entries: [
    { id: 'l1', timestamp: '2026-10-12T09:00:11Z', level: 'INFO', source: 'engine', message: 'run started: 12 targets across 5 batches' },
    { id: 'l2', timestamp: '2026-10-12T09:00:14Z', level: 'INFO', source: 'engine', message: 'batch 1/5 completed: 12/12 succeeded' },
    { id: 'l3', timestamp: '2026-10-12T09:01:02Z', level: 'WARN', source: 'channel', message: 'host 10.0.1.14 ssh handshake slow (1.8s)' },
    { id: 'l4', timestamp: '2026-10-12T09:01:44Z', level: 'ERROR', source: 'engine', message: 'batch 2/5: host 10.0.1.14 apply failed (exit 1)' },
    { id: 'l5', timestamp: '2026-10-12T09:01:45Z', level: 'INFO', source: 'engine', message: 'batch 2/5 compensation started' },
  ],
  truncated: true,
}

// The cluster payload is written in the SERVER's spelling, and that is the
// point of this fixture: /system/cluster-status is marshalled by the gateway
// from `state.ClusterNode` / `state.AssignmentSummary` with encoding/json, so
// the keys are those structs' json tags (`last_heartbeat`, `node_load`,
// `total_active`) and a heartbeat is a time.Time, which reaches the wire as an
// RFC3339 string. Measured against a real daemon:
//
//   {"backend":"sqlite","nodes":null,"summary":{"counts":{},"node_load":{},
//    "total_active":0}}
//
// This fixture used to say `lastHeartbeat: now - 5` (camelCase, and an epoch
// SECONDS number) with no `node_load` at all. The page then read camelCase,
// agreed with the fixture, and stayed blank against the real server — a mock
// that drifts from the wire does not merely fail to test the UI, it vouches
// for a contract the product does not have.
const rfc3339 = (epochSeconds) => new Date(epochSeconds * 1000).toISOString()

const cluster = {
  backend: 'sqlite',
  // Coordination is a separate axis from storage, and the fixture has to carry
  // both: the default deployment is SQLite without coordination, and the two
  // cluster modes below turn coordination on while only changing the registry.
  cluster_enabled: false,
  nodes: [
    { id: 'node-a', role: 'master', status: 'active', address: '10.0.1.11:8080', last_heartbeat: rfc3339(now - 5), joined_at: rfc3339(now - 86400), capabilities: '["shell","pkg","file"]' },
    { id: 'node-b', role: 'worker', status: 'active', address: '10.0.1.12:8080', last_heartbeat: rfc3339(now - 8), joined_at: rfc3339(now - 86400), capabilities: '["shell","file"]' },
    { id: 'node-c', role: 'worker', status: 'offline', address: '10.0.1.13:8080', last_heartbeat: rfc3339(now - 900), joined_at: rfc3339(now - 3600), capabilities: '[]' },
  ],
  summary: {
    counts: { done: 18, interrupted: 1 },
    node_load: { 'node-a': 1, 'node-b': 1 },
    total_active: 2,
  },
}

// A goal-sized fleet for the map's windowing check: 300 hosts x 12 batches.
// The browser check asserts the DOM stays flat as this number grows, so the
// fixture is deliberately big enough to matter.
const FLEET_BATCHES = 12
const FLEET_HOSTS_PER_BATCH = 25

function fleetPlan(per) {
  const batches = []
  for (let b = 1; b <= FLEET_BATCHES; b++) {
    const hosts = []
    for (let h = 0; h < per; h++) hosts.push(`10.${b}.${h + 1}.11`)
    batches.push({ index: b, hosts })
  }
  return { batches, planHash: 'v2:fleet' }
}

function fleetBatchStatus(per) {
  const batches = []
  for (let b = 1; b <= FLEET_BATCHES; b++) {
    batches.push({
      batch_no: b, status: b <= 8 ? 'completed' : b === 9 ? 'running' : 'pending',
      total_hosts: per, succeeded: b <= 8 ? per : b === 9 ? 11 : 0, failed: b === 3 ? 2 : 0,
    })
  }
  return { run_id: 'run-fleet', total_batches: FLEET_BATCHES, done_batches: 8, current_batch_no: 9, batches, steps: [] }
}

// The alert ring's snapshot, in the SSE frame's spelling: json.Marshal of
// alert.Alert, so severity/status are the numeric enums and times are
// RFC3339. One critical (severity 2) firing alert is what makes the header
// badge read 1 — the browser check asserts that, because a stream that
// connects but delivers nothing would otherwise look the same as one that
// works.
const alerts = [
  {
    id: 'alert-1', source: 'engine', severity: 2, status: 0,
    title: '批 2 应用失败', description: 'host 10.0.1.14: connection refused (exit 1)',
    target: 'host:10.0.1.14', starts_at: rfc3339(now - 2400), labels: { run_id: 'run-1000' },
  },
  {
    id: 'alert-2', source: 'channel', severity: 1, status: 0,
    title: 'SSH 握手缓慢', description: 'host 10.0.1.14 handshake 1.8s',
    target: 'host:10.0.1.14', starts_at: rfc3339(now - 3000),
  },
]

// An SSE response: default (unnamed) frames — that is what both consumers
// listen on (useAlertStream and MonitorView use onmessage, not addEventListener)
// — then a connection that stays open, because replay-then-tail is the
// product's shape and closing would make EventSource reconnect forever.
function sse(res, frames) {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
    // The same CORS allowance send() carries: the browser check drives /__mode
    // from a page origin that is not the mock's.
    'Access-Control-Allow-Origin': '*',
  })
  for (const f of frames) res.write(`data: ${JSON.stringify(f)}\n\n`)
}

function send(res, obj, status) {
  const body = JSON.stringify(obj)
  res.writeHead(status || 200, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
    // CORS, so the browser check can drive /__mode from the page it loaded off
    // the dev server (a different origin) without a second server.
    'Access-Control-Allow-Origin': '*',
  })
  res.end(body)
}

// A switchable mode, so the browser check can exercise several behaviours
// without a second server:
//   ok|empty|forbidden — the three list tiers (GET /__mode?mode=...)
//   cluster           — coordination on with a populated registry: node cards.
//   cluster-empty     — coordination on with NOTHING registered: the incident
//                       the page could not name while it read coordination
//                       off `backend`, because that shape is indistinguishable
//                       from a single-node PostgreSQL deployment on the storage
//                       axis alone.
// The default is SQLite with coordination off on purpose: that is the default
// deployment, and the hint it shows is behaviour worth testing rather than a
// case to skip.
let mode = 'ok'

const server = http.createServer((req, res) => {
  const u = req.url.split('?')[0]

  if (u === '/__mode') {
    const m = new URL(req.url, 'http://x').searchParams.get('mode')
    if (m === 'forbidden' || m === 'empty' || m === 'ok' || m === 'cluster' || m === 'cluster-empty') mode = m
    return send(res, { mode })
  }

  // The list endpoints honour the failure mode so the three tiers are testable.
  const scoped = (ok) => {
    if (mode === 'forbidden') {
      res.writeHead(403, { 'Content-Type': 'application/json' })
      return res.end(JSON.stringify({ error: 'session belongs to another user' }))
    }
    if (mode === 'empty') return send(res, { changes: [], nextPageToken: '', totalSize: 0 })
    return send(res, ok)
  }

  if (u === '/api/v1/changes') return scoped({ changes, nextPageToken: '', totalSize: 42 })
  if (u === '/api/v1/templates') return send(res, { templates: [], nextPageToken: '', totalSize: 0 })
  if (u === '/api/v1/targets') return send(res, { targets: [], nextPageToken: '', totalSize: 0 })
  if (u === '/api/v1/audit/log') return send(res, { entries: [], nextPageToken: '', totalSize: 0 })
  if (u === '/api/v1/system/status') {
    return send(res, {
      version: 'v1.21.0', uptime: 86400 * 12, activeRuns: 3, pausedRuns: 1,
      backend: 'sqlite', warnings: ['notify.webhook 未配置'], components: [],
    })
  }
  if (u === '/api/v1/system/batch-status') return send(res, fleetBatchStatus(FLEET_HOSTS_PER_BATCH))
  if (u === '/api/v1/system/cluster-status') {
    // Three deployments the cluster page has to tell apart, and storage alone
    // does not tell them: SQLite without coordination (the default), a live
    // cluster with nodes, and a cluster whose registry is empty.
    let body = cluster
    if (mode === 'cluster') {
      body = { ...cluster, backend: 'postgres', cluster_enabled: true }
    } else if (mode === 'cluster-empty') {
      body = {
        ...cluster, backend: 'postgres', cluster_enabled: true,
        // Wire spelling, not the DTO's: `total_active`/`node_load` are what the
        // gateway actually sends (#153), and a fixture that invents a camelCase
        // summary re-creates the silent-default bug the contract guard now catches.
        nodes: [], summary: { total_active: 0, counts: {}, node_load: {} },
      }
    }
    // ?capability=<name> narrows the node list, the way the gateway does. The
    // authority on what a declaration means is internal/capability (its Go tests
    // pin that); this replica is here only so the browser check can watch the
    // page ask, and can tell "registry empty" apart from "nothing declares X".
    const want = new URL(req.url, 'http://x').searchParams.get('capability')
    if (want === null || want.trim() === '') return send(res, body)
    const declared = (raw) => {
      try {
        const parsed = JSON.parse(raw || '[]')
        return Array.isArray(parsed) && parsed.includes(want.trim())
      } catch {
        return false
      }
    }
    return send(res, { ...body, nodes: body.nodes.filter((n) => declared(n.capabilities)) })
  }
  if (u === '/api/v1/system/auth-info') return send(res, { oidcEnabled: false, githubEnabled: false })
  if (u === '/api/v1/system/config') {
    return send(res, { format: 'yaml', sourcePath: '/etc/levee/config.yaml', content: 'server:\n  port: 8080\n', loadedAt: now })
  }
  if (u === '/api/v1/system/change-trend') {
    const days = []
    const base = Date.UTC(2026, 9, 1)
    for (let i = 13; i >= 0; i--) {
      const d = new Date(base - i * 86400000)
      const o = trendShape[13 - i]
      const total = Object.values(o).reduce((a, b) => a + b, 0)
      days.push({ day: d.toISOString().slice(0, 10), total, outcomes: o })
    }
    return send(res, { buckets: days, series: ['success', 'active', 'rolled_back', 'failed', 'stopped'], truncated: false })
  }

  // Takeover settlements: two events, one still interrupted and one since
  // resumed, so the browser check can assert the timeline reports the run's
  // CURRENT status rather than the moment of settlement.
  if (u === '/api/v1/system/takeover-events') {
    const now = Math.floor(Date.now() / 1000)
    return send(res, {
      events: [
        { id: 'trc-tk-mock-2', run_id: 'run-1002', actor: 'cluster-takeover', timestamp: now - 7200, status: 'running' },
        { id: 'trc-tk-mock-1', run_id: 'run-1003', actor: 'cluster-takeover', timestamp: now - 18000, status: 'interrupted' },
      ],
      days: 7,
      window_from: now - 7 * 86400,
    })
  }

  // One change's worth of detail pages.
  if (/^\/api\/v1\/changes\/run-1000$/.test(u)) return send(res, detail)
  if (/^\/api\/v1\/changes\/run-1000\/trace/.test(u)) return send(res, trace)
  if (/^\/api\/v1\/changes\/run-1000\/approvals/.test(u)) return send(res, { approvals })
  if (/^\/api\/v1\/changes\/run-1000\/logs/.test(u)) return send(res, logs)
  if (/^\/api\/v1\/changes\/run-1000\/plan$/.test(u)) return send(res, fleetPlan(5))

  // The fleet run used by the map's windowing assertion.
  if (u === '/api/v1/changes/run-fleet') {
    return send(res, {
      id: 'run-fleet', label: '大规模 Fleet 变更', status: 'running', environment: 'prod',
      team: '平台组', templateName: 'fleet-rollout', priority: 'high', creator: 'liwg',
      createdAt: now - 7200, updatedAt: now - 60, planHash: 'v2:fleet', params: { fleet: 'cn-east-1' },
    })
  }
  if (u === '/api/v1/changes/run-fleet/plan') return send(res, fleetPlan(FLEET_HOSTS_PER_BATCH))

  // The two SSE streams the console holds open. These used to fall through to
  // the 404 below, which made every page's EventSource fail — invisibly in
  // Chrome (an EventSource error is a network event, never a page error) and
  // loudly in Firefox ("Firefox can’t establish a connection to the server"),
  // which is exactly the false green a second engine exists to catch: the
  // backend has both endpoints and the mock now does too.
  if (u === '/api/v1/alerts/stream') return sse(res, alerts)
  if (/^\/api\/v1\/changes\/run-1000\/events$/.test(u)) {
    // One framed event: MonitorView answers a message by re-reading the
    // change/summary/logs snapshots, and a single frame exercises that path
    // without inventing a live tail for a fixture run.
    return sse(res, [{ type: 'status', run_id: 'run-1000', status: 'running' }])
  }

  send(res, { error: 'mock: no route for ' + u }, 404)
})

const port = Number(process.env.MOCK_PORT || 4599)
server.listen(port, '127.0.0.1', () => {
  console.log(`mock gateway listening on 127.0.0.1:${port}`)
})
