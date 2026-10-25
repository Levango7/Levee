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
//   internal/state.StepStatusNames()、internal/state.ApprovalStatusNames()、
//   internal/state.AssignResultNames()、internal/approval.StatusNames()、
//   internal/cluster.NodeRoleNames() / NodeStatusNames()、
//   internal/conversation.SessionStateNames()
//
// 这些表此前是前端手抄件，由 Go 侧解析抄件再与 owner 比对来守卫。那正是
// #153 的失败形状：守卫核对的是自己的抄写，四处产物互相自洽、一起与服务器
// 不一致，全程没有一处报错。现在值集合只有一份，且是机器产出的。
//
// 同名不同物的一族（batch / assignment / step 三张表都拼 pending、running、
// failed；两张审批表都拼 pending、approved、rejected）刻意是一张表一个类型名，
// 别在这里合并——合并会让一张表能读出另一张表读不到的值，理由写在各自 owner 的
// 清单注释里。

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

/** 步骤状态（steps.status）。与 BATCH_STATES 只在拼写上重叠：步骤的终态是 success，批次的是 completed。pending / running 今天没有写入方，仍列出来是为了让未来的写入方不能给已命名的状态另造一个词。 */
export const STEP_STATES = [
  'pending',
  'running',
  'success',
  'failed',
  'skipped',
] as const

/** StepStatus 的成员集合，由上面那份数组派生。 */
export type StepStatus = (typeof STEP_STATES)[number]

/** RUN 行上的审批结论（runs.approval_status），按写入方清点。schema.sql 对本列注释里的 timeout / skipped 属于下面那张表，不是这里。 */
export const RUN_APPROVAL_STATUSES = [
  'pending',
  'approved',
  'rejected',
] as const

/** RunApprovalStatus 的成员集合，由上面那份数组派生。 */
export type RunApprovalStatus = (typeof RUN_APPROVAL_STATUSES)[number]

/** 审批记录自身的状态（approvals.status）。expired 由 CheckExpiry 产生，而它现在有生产调用方（serve 侧的过期扫描器，见 internal/approval/expiry.go）——在它出现之前超时审批会永远停在 pending，那段空白已由自动过期收口（见 internal/approval/vocabulary.go）。 */
export const APPROVAL_STATUSES = [
  'pending',
  'approved',
  'rejected',
  'expired',
] as const

/** ApprovalStatus 的成员集合，由上面那份数组派生。 */
export type ApprovalStatus = (typeof APPROVAL_STATUSES)[number]

/** 派发结果（assignments.result）。本列的 SQL 默认是 ''，那是「还没有结论」而不是第四个结论，所以不在表里：把它折进 completed 等于声称完成了没发生的工作。 */
export const ASSIGN_RESULTS = [
  'completed',
  'failed',
  'rolled_back',
] as const

/** AssignResult 的成员集合，由上面那份数组派生。 */
export type AssignResult = (typeof ASSIGN_RESULTS)[number]

/** 告警级别（alert.Alert 的 Severity），按紧迫度升序：info → warning → critical。来源是各适配器（nagios/zabbix/prometheus）解析后的归一值，不是监控系统的原始措辞。 */
export const ALERT_SEVERITIES = [
  'info',
  'warning',
  'critical',
] as const

/** AlertSeverity 的成员集合，由上面那份数组派生。 */
export type AlertSeverity = (typeof ALERT_SEVERITIES)[number]

// END GENERATED: vocabulary
