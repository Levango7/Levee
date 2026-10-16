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
// 归属：internal/runstatus.All（运行状态，生命周期顺序）、
//       internal/state.PriorityNames()（优先级，按紧迫度从低到高）
//
// 这两张表此前是前端手抄件，由 Go 侧解析抄件再与 owner 比对来守卫。那正是
// #153 的失败形状：守卫核对的是自己的抄写，四处产物互相自洽、一起与服务器
// 不一致，全程没有一处报错。现在值集合只有一份，且是机器产出的。

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

// END GENERATED: vocabulary
