// Type definitions mirroring the LEVEE protobuf messages. Kept in sync with
// proto/levee.proto so the frontend gets static typing without generating
// TypeScript from the .proto file (which would require an extra build step).

export type ChangeStatus =
  | 'planned'
  | 'pending_approval'
  | 'approved'
  | 'running'
  | 'paused'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'rolled_back'
  | 'archived'

export type Priority = 'low' | 'normal' | 'high' | 'urgent'

export interface Change {
  id: string
  label: string
  status: ChangeStatus
  priority: Priority
  workflowFile: string
  templateName: string
  params: Record<string, string>
  createdAt: number
  updatedAt: number
  createdBy: string
  requester?: string
  team: string
  environment: string
}

export interface Batch {
  index: number
  hosts: string[]
  maxConcurrency: number
  waitAfter: number
}

export interface Plan {
  changeId: string
  targetHosts: string[]
  batches: Batch[]
  impactSummary: string
  estimatedDuration: number
  potentialConflicts: string[]
}

export interface Target {
  id: string
  hostname: string
  channelType: 'ssh' | 'winrm'
  port: number
  credentialRef: string
  labels: Record<string, string>
  reachable: boolean
}

export interface Template {
  name: string
  description: string
  workflowContent: string
  requiredParams: string[]
  createdAt: number
  updatedAt: number
}

export interface TraceEntry {
  id: string
  runId: string
  action: string
  targetHost: string
  input: string
  output: string
  durationMs: number
  timestamp: number
  prevHash: string
  currHash: string
  // actor is populated by the audit log for approval/reject events; not present in raw TraceEntry proto.
  actor?: string
  // detail provides additional context for audit events (e.g. approval comment).
  detail?: string
}

export interface LogEntry {
  runId: string
  timestamp: string
  level: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR'
  message: string
  source: string
}

export interface ChangeEvent {
  changeId: string
  eventType: string
  oldStatus: string
  newStatus: string
  message: string
  timestamp: number
}

export interface VersionInfo {
  version: string
  gitCommit: string
  buildDate: string
  goVersion: string
}

export interface SystemStatus {
  status: 'healthy' | 'degraded' | 'unhealthy'
  activeRuns: number
  pausedRuns: number
  uptimeSeconds: number
  storeType: string
  warnings: string[]
}

export interface PageResult<T> {
  items: T[]
  nextPageToken: string
  totalSize: number
}

// AuditPageResult mirrors the audit API response shape where the proto field
// is "entries" not "items". Used for /audit/log and /audit/traces responses.
export interface AuditPageResult<T> {
  entries: T[]
  nextPageToken: string
  totalSize: number
}

export interface ApiError {
  code: number
  message: string
  details?: unknown
}