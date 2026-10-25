// Domain API wrappers. Each LEVEE service gets a small object with typed
// methods so views can call e.g. `changesApi.list({...})` instead of raw
// `get('/changes')`. The wrappers are thin: they only translate between the
// protobuf-style camelCase JSON used by grpc-gateway and our TS types.
import { del, get, post, put } from './client'
import type {
  ApprovalStatus,
  Change,
  ChangeStatus,
  LogEntry,
  PageResult,
  Plan,
  StepStatus,
  SystemStatus,
  Target,
  Template,
  TraceEntry,
  VersionInfo,
} from '@/types/levee'

// ---------------------------------------------------------------------------
// List response normalisation
// ---------------------------------------------------------------------------

// Raw protojson list responses key the item array by the proto field name:
// /changes -> {changes}, /templates -> {templates}, /targets -> {targets},
// /audit/* -> {entries}. Views consume the uniform PageResult<T> shape with
// `items`, so every list wrapper maps its raw key into `items` here.
type RawListKey = 'changes' | 'templates' | 'targets' | 'entries' | 'agents' | 'groups'

interface RawListEnvelope {
  nextPageToken?: string
  totalSize?: number
  [key: string]: unknown
}

function toPageResult<T>(raw: RawListEnvelope, key: RawListKey): PageResult<T> {
  const items = (raw[key] ?? raw.items ?? []) as T[]
  return {
    items,
    nextPageToken: raw.nextPageToken || '',
    totalSize: raw.totalSize || 0,
  }
}

// ---------------------------------------------------------------------------
// ChangeService
// ---------------------------------------------------------------------------

export interface ListChangesParams {
  /** Zero or more statuses; serialized as a single comma-joined `status` query param. */
  status?: ChangeStatus[]
  labelContains?: string
  pageSize?: number
  pageToken?: string
}

// The backend only accepts status (comma-split server-side), labelContains,
// pageSize and pageToken; team/environment/sortBy/sortOrder params were removed.
function changesListQuery(params: ListChangesParams): Record<string, string | number | undefined> {
  const status = params.status && params.status.length > 0 ? params.status.join(',') : undefined
  return {
    status,
    labelContains: params.labelContains,
    pageSize: params.pageSize,
    pageToken: params.pageToken,
  }
}

/**
 * The 200 body of a one-tap deep-link decision. `status` is "approved" when
 * the quorum completed, "recorded; quorum pending" when it did not (the vote
 * is durable; the run is untouched) — never a fabricated "approved".
 */
export interface DeeplinkDecision {
  status: string
}

/**
 * The 409 body of a tap whose token was already spent on a decided change.
 * The gateway sends it instead of a flat 401 precisely so the mobile page can
 * say "already approved by X" rather than sending the operator round again;
 * see utils/deeplinkEvidence.ts for how a rejected response reaches us.
 * `decided_at` is Unix seconds, the same shape every other REST timestamp in
 * this client carries (formatTimestamp reads it directly).
 */
export interface DeeplinkEvidence {
  /** Always true in this body: the flag is what separates it from a flat 401. */
  decided?: boolean
  /** The record's terminal status: "approved" or "rejected". */
  decision?: string
  run_id?: string
  decided_by?: string
  decided_at?: number
}

export const changesApi = {
  list: (params: ListChangesParams): Promise<PageResult<Change>> =>
    get<RawListEnvelope>('/changes', { params: changesListQuery(params) }).then((raw) =>
      toPageResult<Change>(raw, 'changes'),
    ),

  // `includePlan` is the only way a plan comes back: the server omits the field
  // unless the request asks (see web/src/api/__contract__/wire.json, where the
  // no-flag response and the with-flag response are two recorded shapes).
  // Passing it costs nothing on the default path because the flag is opt-in, but
  // asking for it on a page that never renders the plan would widen every
  // response for no reader — so callers that want the plan say so here.
  get: (id: string, opts?: { includePlan?: boolean }): Promise<Change> =>
    get<Change>(`/changes/${id}`, {
      params: opts?.includePlan === true ? { includePlan: true } : undefined,
    }),

  plan: (id: string, targetHosts?: string[], dryRun?: boolean): Promise<Plan> =>
    post<Plan>(`/changes/${id}/plan`, { targetHosts, dryRun }),

  apply: (id: string, body: { autoApprove?: boolean; maxConcurrency?: number; dryRun?: boolean }): Promise<{
    change: Change
    plan: Plan
    runId: string
    success: boolean
    message: string
  }> => post(`/changes/${id}/apply`, body),

  approve: (id: string, body: { approver: string; comment?: string }): Promise<Change> =>
    post<Change>(`/changes/${id}/approve`, body),

  reject: (id: string, body: { rejecter: string; reason: string }): Promise<Change> =>
    post<Change>(`/changes/${id}/reject`, body),

  approveViaDeepLink: (token: string): Promise<DeeplinkDecision> =>
    post<DeeplinkDecision>('/changes/deeplink/approve', { token }),

  rejectViaDeepLink: (token: string): Promise<DeeplinkDecision> =>
    post<DeeplinkDecision>('/changes/deeplink/reject', { token }),

  pause: (id: string, reason?: string): Promise<Change> =>
    post<Change>(`/changes/${id}/pause`, { reason }),

  resume: (id: string, reason?: string): Promise<Change> =>
    post<Change>(`/changes/${id}/resume`, { reason }),

  cancel: (id: string, body: { reason?: string; force?: boolean }): Promise<Change> =>
    post<Change>(`/changes/${id}/cancel`, body),

  retry: (id: string, body: { replan?: boolean; targetHosts?: string[] }): Promise<Change> =>
    post<Change>(`/changes/${id}/retry`, body),

  rollback: (id: string, body: { runId?: string; dryRun?: boolean; autoApprove?: boolean }): Promise<{
    change: Change
    rollbackRunId: string
    success: boolean
    rolledBackHosts: string[]
    skippedHosts: string[]
    message: string
  }> => post(`/changes/${id}/rollback`, body),

  archive: (id: string, purgeArtifacts?: boolean): Promise<Change> =>
    post<Change>(`/changes/${id}/archive`, { purgeArtifacts }),

  // One approval CHAIN per change: who was asked, the quorum, every decision.
  // The chain was internal-only, so the console could say "awaiting approval"
  // and nothing about why a vote would not land.
  approvals: (id: string): Promise<ApprovalChain[]> =>
    get<{ approvals: ApprovalChain[] }>(`/changes/${id}/approvals`).then((raw) => raw.approvals ?? []),

  logs: (id: string, params?: { runId?: string; levels?: string[]; limit?: number }): Promise<{
    entries: LogEntry[]
    truncated: boolean
  }> => get(`/changes/${id}/logs`, { params }),

  trace: (id: string, params?: { runId?: string; verify?: boolean }): Promise<{
    runId: string
    entries: TraceEntry[]
    hashChainValid: boolean
    verificationMessage: string
  }> => get(`/changes/${id}/trace`, { params }),
}

// ---------------------------------------------------------------------------
// TemplateService
// ---------------------------------------------------------------------------

export const templatesApi = {
  list: (params?: { nameContains?: string; pageSize?: number; pageToken?: string }): Promise<PageResult<Template>> =>
    get<RawListEnvelope>('/templates', { params }).then((raw) => toPageResult<Template>(raw, 'templates')),

  get: (name: string): Promise<Template> => get<Template>(`/templates/${encodeURIComponent(name)}`),

  create: (body: {
    name: string
    description?: string
    workflowContent: string
    requiredParams?: string[]
    overwrite?: boolean
  }): Promise<Template> => post<Template>('/templates', body),

  delete: (name: string, force?: boolean): Promise<void> =>
    del<void>(`/templates/${encodeURIComponent(name)}`, { params: { force } }),

  instantiate: (body: {
    templateName: string
    label: string
    params?: Record<string, string>
    team?: string
    environment?: string
    priority?: string
    dryRun?: boolean
  }): Promise<Change> => post<Change>('/templates/instantiate', body),
}

// ---------------------------------------------------------------------------
// TargetService
// ---------------------------------------------------------------------------

export interface ListTargetsParams {
  /** Label filter as key/value pairs; serialized to the single comma-joined `k=v,k2=v2` form the backend accepts. */
  labelSelector?: Record<string, string>
  channelType?: string
  reachableOnly?: boolean
  pageSize?: number
  pageToken?: string
}

function targetsListQuery(params?: ListTargetsParams): Record<string, string | number | boolean | undefined> {
  if (!params) return {}
  const query: Record<string, string | number | boolean | undefined> = {
    channelType: params.channelType,
    reachableOnly: params.reachableOnly === undefined ? undefined : String(params.reachableOnly),
    pageSize: params.pageSize,
    pageToken: params.pageToken,
  }
  const selector = Object.entries(params.labelSelector ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(',')
  if (selector) {
    query.labelSelector = selector
  }
  return query
}

export const targetsApi = {
  list: (params?: ListTargetsParams): Promise<PageResult<Target>> =>
    get<RawListEnvelope>('/targets', { params: targetsListQuery(params) }).then((raw) =>
      toPageResult<Target>(raw, 'targets'),
    ),

  get: (id: string): Promise<Target> => get<Target>(`/targets/${id}`),

  add: (body: {
    hostname: string
    channelType: 'ssh' | 'winrm'
    port: number
    credentialRef?: string
    labels?: Record<string, string>
    id?: string
  }): Promise<Target> => post<Target>('/targets', body),

  remove: (id: string, force?: boolean): Promise<void> =>
    del<void>(`/targets/${id}`, { params: { force } }),

  // fresh / timeoutSeconds are read from the query string by the backend,
  // never from the POST body.
  check: (id: string, fresh?: boolean, timeoutSeconds?: number): Promise<{
    target: Target
    reachable: boolean
    latencyMs: number
    error: string
    checkedAt: number
  }> => post(`/targets/${id}/check`, {}, { params: { fresh, timeoutSeconds } }),

  // Atomic YAML inventory import (InventoryService.ImportTargets). Unlike the
  // legacy CSV loop below, one request carries the whole file: the backend
  // previews label moves (a row that would leave its registered environment
  // needs admin on THAT environment) and refuses before writing anything, so a
  // refused file imports none of its hosts.
  import: (yaml: string, defaultGroup?: string): Promise<ImportResult> =>
    post<ImportResult>('/targets/import', { yaml, default_group: defaultGroup }),

  // Per-host change history. The backend resolves the target id to its
  // hostname before querying — the proto's history is keyed by ADDRESS.
  history: (id: string, limit?: number): Promise<TargetHistoryEntry[]> =>
    get<TargetHistoryEntry[]>(`/targets/${id}/history`, { params: { limit } }),

  setStatus: (id: string, status: 'active' | 'frozen' | 'retired'): Promise<{ id: string; status: string }> =>
    post(`/targets/${id}/status`, { status }),
}

// Host groups (InventoryService). Flat list — the backend has no paging here.
export interface HostGroup {
  id: string
  name: string
  parent_id?: string
}

export interface ImportResult {
  created: number
  updated: number
  failed: number
  errors: string[]
}

export interface TargetHistoryEntry {
  run_id: string
  workflow_name: string
  status: string
  creator: string
  created_at: number
}

export const groupsApi = {
  list: (): Promise<HostGroup[]> => get<HostGroup[]>('/groups'),
  create: (name: string, parentId?: string): Promise<HostGroup> =>
    post<HostGroup>('/groups', { name, parent_id: parentId }),
  remove: (id: string): Promise<void> => del<void>(`/groups/${id}`),
}

// Agent registry reads (AgentService). The three protocol writes
// (register/heartbeat/deregister) are gRPC-only by design — an agent speaks
// gRPC with its credential; RemoveAgent IS exposed (operator-facing) but its
// refusal comes back as 200 with removed=false plus the reason, which is why
// this wrapper returns the whole reply instead of throwing.
export interface AgentRecord {
  id: string
  address: string
  capabilities: string[]
  status: string
  last_heartbeat_unix?: number
}

export interface RemoveAgentReply {
  removed: boolean
  refusal: string
}

export const agentsApi = {
  list: (params?: { capability?: string; status?: string }): Promise<AgentRecord[]> =>
    get<AgentRecord[]>('/agents', { params }),
  get: (id: string): Promise<AgentRecord> => get<AgentRecord>(`/agents/${id}`),
  remove: (id: string): Promise<RemoveAgentReply> => del<RemoveAgentReply>(`/agents/${id}`),
}

// ---------------------------------------------------------------------------
// AuditService
// ---------------------------------------------------------------------------

export const auditApi = {
  log: (params: {
    changeId?: string
    runId?: string
    actor?: string
    action?: string
    since?: number
    until?: number
    pageSize?: number
    pageToken?: string
  }): Promise<PageResult<TraceEntry>> =>
    get<RawListEnvelope>('/audit/log', { params }).then((raw) => toPageResult<TraceEntry>(raw, 'entries')),

  traces: (params: {
    changeId?: string
    runIds?: string[]
    since?: number
    until?: number
    pageSize?: number
    pageToken?: string
  }): Promise<PageResult<TraceEntry>> =>
    get<RawListEnvelope>('/audit/traces', { params }).then((raw) => toPageResult<TraceEntry>(raw, 'entries')),

  verifyHashChain: (params: { runId?: string; changeId?: string }): Promise<{
    valid: boolean
    entriesVerified: number
    brokenEntryId: string
    brokenReason: string
    runs: Array<{
      runId: string
      valid: boolean
      entriesVerified: number
      brokenEntryId: string
      brokenReason: string
    }>
  }> => get('/audit/verify', { params }),
}

// ---------------------------------------------------------------------------
// SystemService
// ---------------------------------------------------------------------------

export const systemApi = {
  version: (): Promise<VersionInfo> => get<VersionInfo>('/system/version'),
  status: (): Promise<SystemStatus> => get<SystemStatus>('/system/status'),
  config: (params?: { redactSecrets?: boolean; section?: string }): Promise<{
    format: string
    content: string
    sourcePath: string
    loadedAt: number
  }> => get('/system/config', { params }),
  doctor: (): Promise<{
    status: string
    checks: Array<{ name: string; status: string; message: string; remediation: string }>
    checkedAt: number
  }> => post('/system/doctor', {}),
  /**
   * The cluster registry: nodes, assignment summary, which backend answers.
   *
   * `capability` narrows `nodes` server-side to the nodes declaring that module
   * (`GET /system/cluster-status?capability=…`). The narrowing happens on the
   * server because what a declaration MEANS belongs to internal/capability, which
   * the registry also writes — a card and a filter that each parse the column
   * their own way is exactly the drift this repo keeps paying to unwind.
   *
   * It narrows `nodes` ONLY. `summary` stays deployment-wide: assignments are
   * placed by load, not by capability, so a filtered page must not present the
   * counts as if they belonged to the filtered set.
   */
  clusterStatus: (params?: { capability?: string }): Promise<ClusterStatus> =>
    get<ClusterStatus>('/system/cluster-status', { params: { capability: params?.capability } }),
  /**
   * Takeover settlements over the window, newest first is the server's job —
   * the rows arrive in whatever order the store returns and the view sorts.
   *
   * `status` is the run's CURRENT status, not the status at settlement time:
   * a run the cluster interrupted and that has since been resumed is a
   * different story from one still sitting interrupted, and a timeline that
   * reports the latter would send the operator chasing a fixed problem.
   */
  takeoverEvents: (params?: {
    days?: number
    limit?: number
  }): Promise<{
    events: TakeoverEventDTO[]
    days: number
    window_from: number
  }> =>
    get('/system/takeover-events', {
      params: { days: params?.days, limit: params?.limit },
    }),
}

/** One cluster takeover settlement, as GET /system/takeover-events returns it. */
export interface TakeoverEventDTO {
  id: string
  run_id: string
  actor: string
  /** Unix seconds. */
  timestamp: number
  status: string
}

export interface ApprovalDecision {
  approver: string
  /** approve | reject */
  action: string
  reason: string
  /** Unix timestamp (seconds); 0 when not set. */
  decidedAt: number
}

export interface ApprovalChain {
  id: string
  changeId: string
  /** standard / high / emergency */
  level: string
  /** The approvals table's own states. `expired` is reachable only once something
   *  drives CheckExpiry, which today has no production caller — the point of
   *  typing this list rather than free text is that `timeout` and `skipped`,
   *  which prose elsewhere used to advertise here, belong to no writer at all. */
  status: ApprovalStatus
  approvers: string[]
  /** Votes the quorum needs before the run may settle. */
  minApprovers: number
  decisions: ApprovalDecision[]
  /** Unix timestamp (seconds); 0 when not set. */
  expiresAt: number
  /** Empty planHash + legacy=true: still settles the run regardless of plan. */
  planHash: string
  legacy: boolean
  initiator: string
  excludeInitiator: boolean
  /** The plan this chain bound is NOT the run's current plan. */
  stale: boolean
}

// --- Cluster status types (mirror internal/state/backend) ---
//
// These spellings are SNAKE_CASE on purpose, and that is not a style choice:
// /system/cluster-status is written by a hand-written gateway handler that
// marshals `state.ClusterNode` / `state.AssignmentSummary` with encoding/json,
// so the wire key is whatever the struct tag says (`last_heartbeat`,
// `total_active`). The camelCase DTOs elsewhere in this file (`createdAt`,
// `prevHash`) are protojson output, whose spelling proto-gen derives from the
// .proto field names. Measured against a real daemon:
//
//   {"backend":"sqlite","nodes":null,
//    "summary":{"counts":{},"node_load":{},"total_active":0}}
//
// Before this note the interfaces said `lastHeartbeat` / `joinedAt` /
// `nodeLoad` / `totalActive` — the four keys the server never sends. A read of
// a missing key is `undefined`, and every consumer here had a default, so the
// cluster page rendered 心跳 as a dash, 活跃分配 as 0 and Worker 负载 as
// "无活跃分配" on every real deployment, with no error anywhere. The committed
// mock emitted the same camelCase as the DTOs, so the browser checks and the
// unit tests were all internally consistent about a shape the server does not
// produce.

export interface ClusterNodeDTO {
  id: string
  address: string
  role: string
  status: string
  last_heartbeat: string
  joined_at: string
  /**
   * JSON-encoded []string of the node's declared capabilities, e.g.
   * `["shell","pkg"]`. `[]` means the node declared none.
   *
   * Read-only on this surface: the cluster page shows what a node declared.
   * Capability-based routing would change dispatcher semantics (who may run
   * what, and what happens when a capable node is busy), so it is a separate
   * decision rather than an implication of the column being readable.
   */
  capabilities: string
}

export interface AssignmentSummaryDTO {
  counts: Record<string, number>
  node_load: Record<string, number>
  total_active: number
}

export interface ClusterStatus {
  nodes: ClusterNodeDTO[]
  summary: AssignmentSummaryDTO
  backend: string
  /** Whether THIS process runs cluster coordination (membership, leases,
   *  dispatch). snake_case: /system/cluster-status writes it as a map key in
   *  internal/grpc/rest.go handleClusterStatus.
   *
   *  It is not the same statement as `backend`. A PostgreSQL deployment can be
   *  a deliberate single node, and a SQLite one can never be a cluster, so
   *  reading coordination off the storage label told one group of operators to
   *  start workers they never meant to run and told the other group nothing
   *  was wrong when its registry was empty. */
  cluster_enabled: boolean
}

export interface BatchProgressDTO {
  batch_no: number
  status: string
  total_hosts: number
  succeeded: number
  failed: number
}

/** One host's row in one batch (per-host progress under /system/batch-status). */
export interface StepRowDTO {
  host: string
  /** The batch NUMBER the row belongs to (resolved server-side).
   *  snake_case: /system/batch-status writes the aggregates and these rows
   *  with JSON tags (see stepPayload in internal/grpc/rest.go) — this field
   *  was camelCase for a while and read back as undefined. */
  batch_no: number
  step_name: string
  /** Generated from internal/state.StepStatusNames() — pending|running|success|failed|skipped. */
  status: StepStatus
  duration_ms: number
  /** Omitted when the engine did not record one. */
  exit_code?: number
}

export interface BatchSummaryDTO {
  batches: BatchProgressDTO[]
  current_batch_no: number
  total_batches: number
  done_batches: number
  /** Absent when the run has no step rows yet. */
  steps?: StepRowDTO[]
}

export const batchApi = {
  batchStatus: (runID: string): Promise<BatchSummaryDTO> =>
    get<BatchSummaryDTO>('/system/batch-status', { params: { run_id: runID } }),
}

// ---------------------------------------------------------------------------
// Change trend (REST-only endpoint; not a proto RPC)
// ---------------------------------------------------------------------------

/** One day's counts by outcome, exactly as the endpoint returns them. */
export interface TrendBucketDTO {
  day: string
  total: number
  outcomes: Record<string, number>
}

export interface TrendDTO {
  buckets: TrendBucketDTO[]
  /** Legend order the chart must use, so it cannot drift per client. */
  series: string[]
  /** True when the window's row scan hit the server-side cap. */
  truncated: boolean
}

export const trendApi = {
  /**
   * days is the window length; env scopes the trend to one environment
   * (empty = the deployment's default_env, which is what the read gate
   * judges); tzOffset is the browser's getTimezoneOffset() so the server
   * buckets days on the OPERATOR's boundary rather than its own.
   */
  changeTrend: (opts: { days?: number; env?: string; tzOffset: number }): Promise<TrendDTO> =>
    get<TrendDTO>('/system/change-trend', {
      params: {
        days: opts.days || undefined,
        env: opts.env || undefined,
        tz_offset: opts.tzOffset,
      },
    }),
}

// ---------------------------------------------------------------------------
// ConversationService
// ---------------------------------------------------------------------------

export interface ConversationMessageDTO {
  id: string
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: string
  action?: { type: string; payload?: Record<string, string> }
}

export interface ConversationSessionDTO {
  id: string
  user_id: string
  alert_id?: string
  state: string
  messages: ConversationMessageDTO[]
  created_at: string
  updated_at: string
}

export interface ConversationReplyDTO {
  text: string
  action_type?: string
  action_payload?: Record<string, string>
  session_id?: string
}

// The conversation REST endpoints wrap every payload in an envelope
// ({session} / {sessions} / {reply}), and the message reply uses the
// engine's wire shape ({text, action?: {type, payload}}) rather than the
// flat ConversationReplyDTO the views consume. Unwrap and normalise here —
// the backend envelope is the established API, so the frontend adapts.
interface ConversationReplyWire {
  text: string
  action?: { type: string; payload?: Record<string, string> }
}

export const conversationApi = {
  newSession: (userID: string, alertID?: string): Promise<ConversationSessionDTO> =>
    post<{ session: ConversationSessionDTO }>('/conversation/sessions', { user_id: userID, alert_id: alertID })
      .then(r => r.session),

  listSessions: (userID: string): Promise<ConversationSessionDTO[]> =>
    get<{ sessions: ConversationSessionDTO[] }>('/conversation/sessions', { params: { user_id: userID } })
      .then(r => r.sessions),

  getSession: (sessionID: string, userID?: string): Promise<ConversationSessionDTO> =>
    get<{ session: ConversationSessionDTO }>(`/conversation/sessions/${sessionID}`, { params: { user_id: userID } })
      .then(r => r.session),

  sendMessage: (sessionID: string, userID: string, text: string): Promise<ConversationReplyDTO> =>
    post<{ reply: ConversationReplyWire }>(`/conversation/sessions/${sessionID}/messages`, { user_id: userID, text })
      .then(r => ({
        text: r.reply.text,
        action_type: r.reply.action?.type,
        action_payload: r.reply.action?.payload,
        session_id: sessionID,
      })),

  closeSession: (sessionID: string, userID?: string): Promise<void> =>
    del(`/conversation/sessions/${sessionID}`, { params: { user_id: userID } }),
}

// Re-export primitive helpers for views that need ad-hoc calls.
export { get, post, put, del }