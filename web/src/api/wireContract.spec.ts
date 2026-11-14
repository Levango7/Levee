import { existsSync, readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// wireContract.spec.ts — the TypeScript half of the DTO contract.
//
// The wire shape is produced by the SERVER (internal/grpc/wire_snapshot_test.go
// writes web/src/api/__contract__/wire.json by driving the real handlers). This
// file reads that snapshot and checks the DTOs in api/index.ts against it, which
// is the only way to catch the failure this pair exists for: the client spelling
// a key the server never sends.
//
// A real instance, this session: the client read `batchNo` while the wire said
// `batch_no`, so `undefined` was pushed into a Set and the monitor's target map
// grew a phantom third column. Nothing on either side failed — the Go test
// proved the numbers, the TS test proved the parsing, and the two agreed on
// nothing.
//
// Two directions are checked:
//
//   1. every key the CLIENT reads must exist on the wire (a missing one is a
//      silently blank cell);
//   2. every key the WIRE sends must be either read by the client or listed in
//      SERVER_ONLY below with a reason (so adding a field server-side does not
//      rot the guard, but adding one nobody notices does not pass either).
//
// A third direction was added after the second cluster incident, and it is the
// one that matters most: the read set is taken from the DTO INTERFACES
// themselves rather than from the hand-written DTO_READS lists, because a list
// that transcribes what the client "should" read proves nothing about what it
// actually reads.

function locatePath(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (existsSync(p)) return p
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

interface WireRow {
  keys?: string[]
  rows?: Record<string, string[]>
  objects?: Record<string, string[]>
  /** Element key set for endpoints whose payload is a bare ARRAY. */
  items?: string[]
}

/** A DTO's read set: `keys` are top-level, `row`+`rowKeys` describe a page's items. */
interface DtoReads {
  row?: string
  keys?: string[]
  rowKeys?: string[]
  /**
   * The interface for the ELEMENTS when the payload itself is an array
   * (/agents, /groups). Those responses have no envelope, so `dto` and
   * `rowDto` describe nothing and the element shape would go unchecked.
   */
  rootDto?: string
  /**
   * The interface in api/index.ts that DECLARES this payload, and the ones for
   * its `row` items and nested objects. These names let the guard read the
   * client's real spelling out of the source instead of trusting the hand-copied
   * `keys`/`rowKeys` lists — see 'the interfaces declare only keys the wire
   * sends', which exists because the lists alone passed while ClusterNodeDTO
   * spelled `lastHeartbeat` and the wire sent `last_heartbeat`.
   */
  dto?: string
  rowDto?: string
  objectDtos?: Record<string, string>
}

/**
 * The field names an `export interface NAME` declares, read out of api/index.ts.
 *
 * Line-anchored rather than brace-split on values: a field is a line at the
 * interface's own indent ending in `:`, which ignores the JSDoc blocks several
 * of these DTOs carry. Depth tracking rather than a single `}` match keeps
 * nested object types (`x: { a: string }`) inside the interface they belong to.
 */
function interfaceFields(name: string, source: string): string[] {
  const header = new RegExp(`export interface ${name}\\s*\\{`)
  const found = header.exec(source)
  if (!found) throw new Error(`api/index.ts declares no "export interface ${name}"`)
  const open = found.index + found[0].length - 1
  let depth = 0
  let close = -1
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++
    else if (source[i] === '}') {
      depth--
      if (depth === 0) {
        close = i
        break
      }
    }
  }
  if (close < 0) throw new Error(`interface ${name} is not closed in api/index.ts`)
  const fields: string[] = []
  for (const line of source.slice(open + 1, close).split('\n')) {
    const m = /^ {2}([A-Za-z_][A-Za-z0-9_]*)(\?)?:/.exec(line)
    if (m) fields.push(m[1]!)
  }
  return fields
}

const wire: Record<string, WireRow> = JSON.parse(readFileSync(locatePath('src/api/__contract__/wire.json'), 'utf8'))
const apiSource = readFileSync(locatePath('src/api/index.ts'), 'utf8')
// Not every payload the console reads is declared here: Change, Plan and Batch
// live in types/levee.ts. api/index.ts is searched FIRST and wins, so adding the
// fallback cannot silently switch which declaration an existing name resolves to
// — it only extends coverage to names the first file does not declare.
const typesSource = readFileSync(locatePath('src/types/levee.ts'), 'utf8')

/** The fields of `name`, from api/index.ts or, if only declared there, types/levee.ts. */
function fieldsOf(name: string): string[] {
  try {
    return interfaceFields(name, apiSource)
  } catch {
    return interfaceFields(name, typesSource)
  }
}

/**
 * What each client DTO reads, per endpoint. `row` names the array field the DTO
 * describes when the payload is a page envelope.
 *
 * Keys are listed the way the CLIENT spells them, because that is the half that
 * drifts: the Go side has a guard for its own spelling.
 */
const DTO_READS: Record<string, DtoReads> = {
  'batch-status': {
    row: 'batches',
    keys: ['batches', 'current_batch_no', 'done_batches', 'total_batches'],
    rowKeys: ['batch_no', 'status', 'total_hosts', 'succeeded', 'failed'],
    dto: 'BatchSummaryDTO',
    rowDto: 'BatchProgressDTO',
  },
  'change-trend': {
    row: 'buckets',
    keys: ['buckets', 'series', 'truncated'],
    rowKeys: ['day', 'total', 'outcomes'],
    dto: 'TrendDTO',
    rowDto: 'TrendBucketDTO',
  },
  changes: {
    row: 'changes',
    keys: ['changes', 'totalSize'],
    rowKeys: ['id', 'label', 'status', 'templateName', 'createdAt', 'updatedAt', 'createdBy', 'workflowFile'],
  },
  change: {
    keys: ['id', 'label', 'status', 'templateName', 'createdAt', 'updatedAt', 'createdBy', 'workflowFile'],
  },
  // The same endpoint one flag up. `plan` is a nested object and `plan.batches`
  // its item array, and both are now read by the detail page — which is why they
  // are in the contract rather than inferred from the proto: a field spelled
  // differently one level down renders as a blank panel, not an error.
  'change-with-plan': {
    keys: ['id', 'label', 'status', 'templateName', 'createdAt', 'updatedAt', 'createdBy', 'workflowFile', 'plan'],
    objectDtos: { plan: 'Plan', 'plan.batches': 'Batch' },
  },
  'audit-log': {
    row: 'entries',
    keys: ['entries', 'totalSize'],
    rowKeys: ['id', 'runId', 'action', 'actor', 'targetHost', 'timestamp', 'prevHash', 'currHash'],
  },
  'cluster-status': {
    keys: ['backend', 'cluster_enabled', 'nodes', 'summary'],
    row: 'nodes',
    rowKeys: ['address', 'capabilities', 'id', 'joined_at', 'last_heartbeat', 'role', 'status'],
    dto: 'ClusterStatus',
    rowDto: 'ClusterNodeDTO',
    objectDtos: { summary: 'AssignmentSummaryDTO' },
  },
  // The rollback POST — the shape the partial-rollback remediation wizard
  // reads its evidence from. It was the one endpoint the contract could not
  // see (the snapshot covered GET endpoints only), which is exactly why
  // `skippedHosts` could go missing from the server without anything here
  // failing: the panel would render blank and no error would be raised.
  //
  // The recorded keys are the union of the preview's and the commit's: the
  // server omits the unset fields (protojson), so a preview carries no
  // `rollbackRunId` while a commit does. The union is what the browser may
  // read from this endpoint.
  rollback: {
    keys: ['change', 'rollbackRunId', 'success', 'rolledBackHosts', 'skippedHosts', 'message'],
    dto: 'RollbackResponseDTO',
  },
  // The DB Change Ticket — the "one sheet" a database change is presented on.
  // Seven section objects, each with its own shape. The contract records the
  // section NAMES here; the fields inside each section are pinned by
  // src/views/TicketView.spec.ts, because protojson omits unset scalars and
  // an empty section (no approval / no rollback declared) would otherwise make
  // a whole-section comparison vacuous.
  'dbchange-ticket': {
    keys: ['change', 'sql', 'rollback', 'window', 'approval', 'execution', 'audit'],
    dto: 'DBChangeTicket',
  },
  // The five endpoints the console calls that the snapshot covered later.
  // /agents and /groups have no envelope: their elements are the payload.
  agents: {
    rootDto: 'AgentRecord',
  },
  groups: {
    rootDto: 'HostGroup',
  },
  // protojson, so camelCase — the same spelling TraceEntry declares.
  'audit-traces': {
    row: 'entries',
    keys: ['entries', 'nextPageToken', 'totalSize'],
    rowKeys: [
      'id', 'runId', 'action', 'targetHost', 'input', 'output', 'durationMs',
      'timestamp', 'prevHash', 'currHash', 'actor', 'detail',
    ],
    rowDto: 'TraceEntry',
  },
  'system-version': {
    keys: ['version', 'gitCommit', 'buildDate', 'goVersion'],
    dto: 'VersionInfo',
  },
  'conversation-sessions': {
    row: 'sessions',
    keys: ['sessions'],
    rowKeys: ['id', 'user_id', 'alert_id', 'state', 'messages', 'created_at', 'updated_at'],
    rowDto: 'ConversationSessionDTO',
  },
}

/**
 * Keys the wire sends that no client reads yet. Each entry must say why, so the
 * list cannot become a place where drift goes to hide: a key that appears here
 * without a reason is a gap someone chose not to close, and the test says so.
 */
const SERVER_ONLY: Record<string, string[]> = {
  'batch-status': ['steps'],
  'change-trend': [],
  changes: [],
  change: [],
  'audit-log': [],
  'cluster-status': [],
  // Every key of the rollback response is read by the remediation wizard.
  rollback: [],
  // The registry reports its own load counters; the console reads status and
  // the heartbeat only, so these are sent but not consumed (yet).
  agents: ['active_tasks', 'completed_tasks', 'failed_tasks', 'max_concurrent', 'registered_at_unix'],
  // The group list is a projection for host pickers: the timestamps and tenant
  // belong to `levee group list`, which reads the stored row over the CLI.
  groups: ['created_at', 'created_at_nanos', 'tenant_id'],
  // Evidence and tenancy columns exist for `audit show` / `audit export`; the
  // timeline renders detail and the chain pair, not the stored JSON verbatim.
  'audit-traces': ['storedDetail', 'tenantId', 'timestampNanos'],
  'system-version': [],
  'conversation-sessions': [],
}

describe('DTO contract: what the client reads is what the server sends', () => {
  it('has a snapshot to check against', () => {
    // Without the Go side's snapshot this file would pass on nothing.
    expect(Object.keys(wire).length).toBeGreaterThan(4)
    for (const name of Object.keys(DTO_READS)) {
      expect(wire, `snapshot must cover ${name}`).toHaveProperty(name)
    }
  })

  it('reads no key the wire does not have', () => {
    for (const [name, dto] of Object.entries(DTO_READS)) {
      const snapshot = wire[name]!
      const wireKeys = snapshot.keys ?? []
      if (dto.rootDto) {
        expect(
          snapshot.items?.length,
          `${name}: the snapshot must carry element keys for a bare array`,
        ).toBeGreaterThan(0)
      }
      for (const key of dto.keys ?? []) {
        expect(wireKeys, `${name} reads top-level ${key}`).toContain(key)
      }
      if (dto.row) {
        const rowKeys = snapshot.rows?.[dto.row]
        expect(rowKeys, `${name}: the snapshot must carry ${dto.row} keys`).toBeTruthy()
        for (const key of dto.rowKeys ?? []) {
          expect(rowKeys, `${name} reads ${dto.row}[].${key}`).toContain(key)
        }
      }
    }
  })

  it('accounts for every key the wire sends', () => {
    for (const [name, dto] of Object.entries(DTO_READS)) {
      const read = new Set([...(dto.keys ?? []), ...(dto.rowKeys ?? [])])
      // A bare-array payload has no hand list: its read set IS the element
      // DTO's declaration, so derive it from the interface rather than
      // duplicating the field names here (a copy would drift silently).
      if (dto.rootDto) {
        for (const field of fieldsOf(dto.rootDto)) read.add(field)
      }
      const known = new Set([...read, ...(SERVER_ONLY[name] ?? [])])
      for (const k of wire[name]!.keys ?? []) {
        expect(known, `${name}: wire key ${k} is declared nowhere`).toContain(k)
      }
      if (dto.row) {
        for (const k of wire[name]!.rows?.[dto.row] ?? []) {
          expect(known, `${name}: ${dto.row}[].${k} is declared nowhere`).toContain(k)
        }
      }
      for (const k of wire[name]!.items ?? []) {
        expect(known, `${name}: item key ${k} is declared nowhere`).toContain(k)
      }
    }
  })

  it('does not let PascalCase onto the wire', () => {
    // The Go side's own guard covers this; repeating it here means a TS-side
    // reader cannot "fix" a mismatch by declaring the PascalCase spelling.
    for (const [name, entry] of Object.entries(wire)) {
      for (const forbidden of entry.keys ?? []) {
        expect(/^[A-Z]/.test(forbidden), `${name}: ${forbidden} is PascalCase`).toBe(false)
      }
      for (const item of entry.items ?? []) {
        expect(/^[A-Z]/.test(item), `${name}: item key ${item} is PascalCase`).toBe(false)
      }
      for (const keys of Object.values(entry.rows ?? {})) {
        for (const k of keys) {
          expect(/^[A-Z]/.test(k), `${name}: row key ${k} is PascalCase`).toBe(false)
        }
      }
    }
  })

  it('the interfaces declare only keys the wire sends', () => {
    // The direction that catches the real incident: a DTO spelling a key the
    // server never sends reads back `undefined`, and every consumer here has a
    // default, so the panel renders blanks and nothing errors.
    //
    // This reads the INTERFACES, not DTO_READS. The table is hand-written, and
    // the previous version of this file only checked that api/index.ts contained
    // the words "export interface" somewhere — so the table could (and did)
    // record `last_heartbeat` while ClusterNodeDTO declared `lastHeartbeat`,
    // and the guard passed on its own transcription. A mock that agreed with the
    // DTO, a unit fixture that agreed with the mock, and a browser check running
    // against that mock made four artifacts consistent with each other and wrong
    // about the server.
    const requireDto = (name: string, iface: string | undefined, sent: string[], where: string) => {
      if (!iface) return
      const declared = fieldsOf(iface)
      expect(declared.length, `${where}: extracted no fields from ${iface} — the check would pass on nothing`).toBeGreaterThan(0)
      for (const field of declared) {
        expect(sent, `${name}: ${iface} declares ${field} but the wire does not send it`).toContain(field)
      }
    }

    let derived = 0
    for (const [name, dto] of Object.entries(DTO_READS)) {
      const snapshot = wire[name]!
      if (dto.dto) derived++
      requireDto(name, dto.dto, snapshot.keys ?? [], name)
      if (dto.row && dto.rowDto) {
        requireDto(name, dto.rowDto, snapshot.rows?.[dto.row] ?? [], `${name}.${dto.row}[]`)
      }
      if (dto.rootDto) {
        requireDto(name, dto.rootDto, snapshot.items ?? [], `${name}.items[]`)
      }
      for (const [field, iface] of Object.entries(dto.objectDtos ?? {})) {
        requireDto(name, iface, snapshot.objects?.[field] ?? [], `${name}.${field}`)
      }
    }
    // A floor, not a style preference: dropping the `dto` names from these
    // entries would otherwise shrink this check to nothing without failing it.
    expect(derived, 'too few DTOs are checked against their declarations').toBeGreaterThanOrEqual(3)
  })

  it('records nested object shapes, not just the top level', () => {
    // Without `objects` in the snapshot the check above is blind one level down,
    // which is exactly where the cluster DTO drifted.
    const cluster = wire['cluster-status']!
    expect(Object.keys(cluster.objects ?? {}), 'cluster-status must record a nested object').toContain('summary')
    for (const [field, keys] of Object.entries(cluster.objects ?? {})) {
      expect(keys.length, `${field} contributed no keys`).toBeGreaterThan(0)
    }
  })
})
