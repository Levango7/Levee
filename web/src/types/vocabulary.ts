// TypeScript value lists owned by Go packages.
//
// Everything between the BEGIN/END GENERATED markers below is written by
// internal/docgen and is overwritten on every run — edit the Go owner instead.
// Text outside the markers is hand-written and is preserved.
//
// Why this file exists: the console's status and priority vocabularies used to
// be hand-copied here, and a Go test parsed that copy to compare it against the
// owner. Parsing a hand copy to check it against the owner cannot see the two
// disagreeing in the way that matters — it can only see the copy disagreeing with
// the parser. #153 paid for that shape once already (three cluster-page readings
// stuck at their defaults while every guard stayed green, because the contract
// test compared a hand-copied table with itself).
//
// Regenerate with: go run ./internal/docgen
// CI enforces freshness with: go run ./internal/docgen -check -root .

// BEGIN GENERATED: vocabulary
// 本节由 internal/docgen 生成，请勿手工编辑，也不要在这里追加手写声明。
// 重新生成：go run ./internal/docgen
// 归属（每张表的值集合与顺序都由它自己那份 owner 决定）：
//   internal/runstatus.All、internal/state.PriorityNames()、
//   internal/state.BatchStateNames()、internal/state.AssignmentStateNames()、
//   internal/cluster.NodeRoleNames() / NodeStatusNames()、
//   internal/conversation.SessionStateNames()
//
// 这些表此前是前端手抄件，由 Go 侧解析抄件再与 owner 比对来守卫。那正是
// #153 的失败形状：守卫核对的是自己的抄写，四处产物互相自洽、一起与服务器
// 不一致，全程没有一处报错。现在值集合只有一份，且是机器产出的。
//
// 同名不同物的两组（batch 与 assignment 都拼 pending / done / interrupted）
// 刻意是两张表、两个类型名，别在这里合并——理由写在 owner 的清单注释里。

/** 变更（run）状态，与 runs.status 列同源。 */
export const CHANGE_STATUSES = [
  'draft',
  'pending',
  'planned',
  'approved',
  'running',
  'paused',
  'completed',
  'failed',
  'cancelled',
  'rolled_back',
  'rolled_back_partial',
  'rollback_incomplete',
  'rejected',
  'archived',
  'interrupted',
] as const

/** ChangeStatus 的成员集合，由上面那份数组派生。 */
export type ChangeStatus = (typeof CHANGE_STATUSES)[number]

/** 创建变更时的优先级主张；不是审批档位（档位见 standard/high/emergency）。 */
export const PRIORITIES = [
  'low',
  'normal',
  'high',
  'urgent',
] as const

/** Priority 的成员集合，由上面那份数组派生。 */
export type Priority = (typeof PRIORITIES)[number]

/** 批次进度。completed 是写入方实际产出的拼写，done 是 schema 注释仍记录的历史拼写。 */
export const BATCH_STATES = [
  'pending',
  'running',
  'completed',
  'done',
  'failed',
  'rolled_back',
  'interrupted',
] as const

/** BatchState 的成员集合，由上面那份数组派生。 */
export type BatchState = (typeof BATCH_STATES)[number]

/** 派发状态。与 BATCH_STATES 只是拼写重叠：这里的 pending 是「还没被 worker 领取」，done 是活跃终态而非历史拼写。 */
export const ASSIGNMENT_STATES = [
  'pending',
  'executing',
  'done',
  'interrupted',
] as const

/** AssignmentState 的成员集合，由上面那份数组派生。 */
export type AssignmentState = (typeof ASSIGNMENT_STATES)[number]

/** 集群节点角色，按权威顺序：主控节点在前。 */
export const NODE_ROLES = [
  'master',
  'worker',
] as const

/** NodeRole 的成员集合，由上面那份数组派生。 */
export type NodeRole = (typeof NODE_ROLES)[number]

/** 集群节点生命周期。leaving 目前没有任何写入方，但仍在这里命名——把三态折成两色会把优雅下线的节点涂成故障。 */
export const NODE_STATUSES = [
  'active',
  'leaving',
  'offline',
] as const

/** NodeStatus 的成员集合，由上面那份数组派生。 */
export type NodeStatus = (typeof NODE_STATUSES)[number]

/** 会话状态，取自 SessionState.String()；末位 unknown 是越界值的兜底拼写，不是状态机里的一个状态。 */
export const SESSION_STATES = [
  'idle',
  'diagnosing',
  'recommending',
  'reviewing',
  'executing',
  'done',
  'failed',
  'unknown',
] as const

/** SessionStateName 的成员集合，由上面那份数组派生。 */
export type SessionStateName = (typeof SESSION_STATES)[number]

// END GENERATED: vocabulary
