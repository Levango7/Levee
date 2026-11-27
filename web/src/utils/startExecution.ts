import type { Change } from '@/types/levee'

const STORAGE_PREFIX = 'levee.start.v1.'
const RECORD_VERSION = 1

interface StartKeyRecord {
  version: number
  key: string
  updatedAt: string
  planJSON: string
}

export function mintStartKey(): string {
  const cryptoApi = globalThis.crypto
  if (typeof cryptoApi?.randomUUID === 'function') {
    return `web-${cryptoApi.randomUUID()}`
  }
  if (typeof cryptoApi?.getRandomValues === 'function') {
    const bytes = new Uint8Array(16)
    cryptoApi.getRandomValues(bytes)
    return `web-${Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')}`
  }
  throw new Error('当前浏览器无法生成安全幂等键，请使用支持 Web Crypto 的浏览器')
}

function storageKey(changeID: string): string {
  return `${STORAGE_PREFIX}${changeID}`
}

function remove(storage: Storage, changeID: string): void {
  storage.removeItem(storageKey(changeID))
}

function validExisting(change: Change, storage: Storage): StartKeyRecord | null {
  const raw = storage.getItem(storageKey(change.id))
  if (!raw) return null
  let record: StartKeyRecord
  try {
    record = JSON.parse(raw) as StartKeyRecord
  } catch {
    remove(storage, change.id)
    return null
  }
  if (record.version !== RECORD_VERSION || !record.key || record.key.length > 128) {
    remove(storage, change.id)
    return null
  }
  // Running proves the server accepted the operation, so the key must stay
  // available for reconnects even though the run's updatedAt keeps moving.
  if (change.status === 'running') return record
  // An approved run with the same timestamp AND exact stored plan is the only
  // uncertain pre-acceptance state. A new plan/approval must get a fresh key,
  // including a replan within the same Unix second; terminal and all other
  // states prove this particular start is no longer retryable.
  if (
    change.status === 'approved' &&
    record.updatedAt === String(change.updatedAt) &&
    record.planJSON === JSON.stringify(change.plan ?? null)
  ) {
    return record
  }
  remove(storage, change.id)
  return null
}

// startKeyFor returns the one durable key for the current operation. Storage
// errors deliberately propagate BEFORE any request is sent: silently falling
// back to a reload-unsafe key could double-execute a change after a disconnect.
export function startKeyFor(
  change: Change,
  storage?: Storage,
  mint: () => string = mintStartKey,
): string {
  const store = storage ?? window.localStorage
  const existing = validExisting(change, store)
  if (existing) return existing.key
  const record: StartKeyRecord = {
    version: RECORD_VERSION,
    key: mint(),
    updatedAt: String(change.updatedAt),
    planJSON: JSON.stringify(change.plan ?? null),
  }
  store.setItem(storageKey(change.id), JSON.stringify(record))
  return record.key
}

// Reconciliation runs after every detail read. Cleanup failure is safe to leave
// for the next read: startKeyFor will fail closed rather than issuing a request
// with an uncertain key. A read-only refresh must not fail because storage did.
export function reconcileStartKey(change: Change, storage?: Storage): void {
  try {
    validExisting(change, storage ?? window.localStorage)
  } catch {
    // Keep the record for startKeyFor's explicit, pre-execution error path.
  }
}
