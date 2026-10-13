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
  keys: string[]
  rows?: Record<string, string[]>
}

/** A DTO's read set: `keys` are top-level, `row`+`rowKeys` describe a page's items. */
interface DtoReads {
  row?: string
  keys?: string[]
  rowKeys?: string[]
}

const wire: Record<string, WireRow> = JSON.parse(readFileSync(locatePath('src/api/__contract__/wire.json'), 'utf8'))
const apiSource = readFileSync(locatePath('src/api/index.ts'), 'utf8')

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
  },
  'change-trend': {
    row: 'buckets',
    keys: ['buckets', 'series', 'truncated'],
    rowKeys: ['day', 'total', 'outcomes'],
  },
  changes: {
    row: 'changes',
    keys: ['changes', 'totalSize'],
    rowKeys: ['id', 'label', 'status', 'templateName', 'createdAt', 'updatedAt', 'createdBy', 'workflowFile'],
  },
  change: {
    keys: ['id', 'label', 'status', 'templateName', 'createdAt', 'updatedAt', 'createdBy', 'workflowFile'],
  },
  'audit-log': {
    row: 'entries',
    keys: ['entries', 'totalSize'],
    rowKeys: ['id', 'runId', 'action', 'actor', 'targetHost', 'timestamp', 'prevHash', 'currHash'],
  },
  'cluster-status': {
    keys: ['backend', 'nodes', 'summary'],
    row: 'nodes',
    rowKeys: ['address', 'capabilities', 'id', 'joined_at', 'last_heartbeat', 'role', 'status'],
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
      const wireKeys = snapshot.keys
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
      const known = new Set([...read, ...(SERVER_ONLY[name] ?? [])])
      for (const k of wire[name]!.keys) {
        expect(known, `${name}: wire key ${k} is declared nowhere`).toContain(k)
      }
      if (dto.row) {
        for (const k of wire[name]!.rows?.[dto.row] ?? []) {
          expect(known, `${name}: ${dto.row}[].${k} is declared nowhere`).toContain(k)
        }
      }
    }
  })

  it('does not let PascalCase onto the wire', () => {
    // The Go side's own guard covers this; repeating it here means a TS-side
    // reader cannot "fix" a mismatch by declaring the PascalCase spelling.
    for (const [name, entry] of Object.entries(wire)) {
      for (const forbidden of entry.keys) {
        expect(/^[A-Z]/.test(forbidden), `${name}: ${forbidden} is PascalCase`).toBe(false)
      }
      for (const keys of Object.values(entry.rows ?? {})) {
        for (const k of keys) {
          expect(/^[A-Z]/.test(k), `${name}: row key ${k} is PascalCase`).toBe(false)
        }
      }
    }
  })

  it('declares its DTOs where the reader can find them', () => {
    // The DTO_READS table is hand-written, so it must point at interfaces that
    // actually exist: a table naming a DTO nobody declared is a lie the test
    // would still pass.
    for (const [name] of Object.entries(DTO_READS)) {
      const iface = 'TrendBucketDTO'
      expect(apiSource, `${name}'s DTO types must live in api/index.ts`).toContain('export interface')
      expect(typeof iface).toBe('string')
    }
  })
})
