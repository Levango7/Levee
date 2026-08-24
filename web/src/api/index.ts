// Domain API wrappers. Each LEVEE service gets a small object with typed
// methods so views can call e.g. `changesApi.list({...})` instead of raw
// `get('/changes')`. The wrappers are thin: they only translate between the
// protobuf-style camelCase JSON used by grpc-gateway and our TS types.
import { del, get, post, put } from './client'
import type {
  Change,
  ChangeStatus,
  LogEntry,
  PageResult,
  Plan,
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
type RawListKey = 'changes' | 'templates' | 'targets' | 'entries'

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

export const changesApi = {
  list: (params: ListChangesParams): Promise<PageResult<Change>> =>
    get<RawListEnvelope>('/changes', { params: changesListQuery(params) }).then((raw) =>
      toPageResult<Change>(raw, 'changes'),
    ),

  get: (id: string): Promise<Change> => get<Change>(`/changes/${id}`),

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

  approveViaDeepLink: (token: string): Promise<{ status: string }> =>
    post<{ status: string }>('/changes/deeplink/approve', { token }),

  rejectViaDeepLink: (token: string): Promise<{ status: string }> =>
    post<{ status: string }>('/changes/deeplink/reject', { token }),

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
}

// Re-export primitive helpers for views that need ad-hoc calls.
export { get, post, put, del }