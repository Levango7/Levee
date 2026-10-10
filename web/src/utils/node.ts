// Node role / status mapping for the cluster page's node cards.
//
// Same reasoning as ./batch.ts and ./assignment.ts, for a different payload: the
// node list comes from `internal/cluster.Node`, whose `Role` and `Status` fields are
// typed string enumerations declared in internal/cluster/node.go:23-43. Before this
// file the page interpolated `n.role` verbatim (so a Chinese page showed
// `master`/`worker`), used the wire string as the dot's tooltip, and folded the
// three states onto two colours with `status === 'active' ? 'ok' : 'bad'` — which
// tells an operator that a node gracefully leaving has *failed*.
//
// Do NOT merge these lists with ./batch.ts or ./assignment.ts. `pending` and `done`
// are spelled the same over there but describe batch/assignment rows; here
// `active`/`leaving`/`offline` say whether a node is registered and reachable at
// all. internal/cluster/node.go keeps them in their own typed groups (NodeStatus /
// NodeRole), and internal/cluster/node_vocabulary_test.go pins both against the
// lists below and checks every tone used here has a real `.lv-dot--*` rule in
// web/src/styles/base.css — so a new node state has to be named and coloured here
// before it can reach an operator.

export const NODE_ROLES = ['master', 'worker'] as const
export type NodeRole = (typeof NODE_ROLES)[number]

export const NODE_STATUSES = ['active', 'leaving', 'offline'] as const
export type NodeStatus = (typeof NODE_STATUSES)[number]

/**
 * Dot tones available in web/src/styles/base.css. The guard test checks the subset
 * used below against that stylesheet, so `warning` (the el-tag word, which reads
 * almost the same as the real class `warn`) is a test failure rather than a dot that
 * renders with no background.
 */
export type DotTone = 'ok' | 'warn' | 'bad' | 'idle' | 'live'

const ROLE_LABEL: Record<NodeRole, string> = {
  master: '主控节点',
  worker: '工作节点',
}

const STATUS_LABEL: Record<NodeStatus, string> = {
  active: '在线',
  leaving: '正在下线',
  offline: '已离线',
}

// `leaving` reads as warn, not bad: the registry asks a node to leave gracefully and
// nothing in internal/cluster writes this value yet (node.go:28-29 only declares it),
// so painting it as a failure would invent an outage the engine never reported.
const TONE_BY_STATUS: Record<NodeStatus, DotTone> = {
  active: 'ok',
  leaving: 'warn',
  offline: 'bad',
}

/**
 * Chinese label for a node role. Unrecognised values pass through verbatim — a role
 * we cannot name is worth seeing, whereas inventing a translation would let a new
 * backend role masquerade as a known one.
 */
export function nodeRoleLabel(role: string): string {
  return ROLE_LABEL[role as NodeRole] ?? role
}

/**
 * Chinese label for a node status, used for the dot's tooltip. Unrecognised values
 * pass through verbatim for the same reason as above.
 */
export function nodeStatusLabel(status: string): string {
  return STATUS_LABEL[status as NodeStatus] ?? status
}

/**
 * Dot tone for a node status. Anything we cannot name falls back to `bad`: for a
 * liveness dot "up" is the only reassuring colour, so an unproven state must not read
 * as healthy.
 */
export function nodeStatusTone(status: string): DotTone {
  return TONE_BY_STATUS[status as NodeStatus] ?? 'bad'
}
