import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { Change } from '@/types/levee'
import { mintStartKey, reconcileStartKey, startKeyFor } from './startExecution'

function change(overrides: Partial<Change> = {}): Change {
  return {
    id: 'run-approved',
    label: 'approved change',
    status: 'approved',
    priority: 'normal',
    workflowFile: 'workflow.yaml',
    templateName: 'deploy',
    params: {},
    createdAt: 100,
    updatedAt: 200,
    createdBy: 'alice',
    team: 'platform',
    environment: 'prod',
    plan: { changeId: 'run-approved', targetHosts: ['web-1'], batches: [], impactSummary: '' },
    ...overrides,
  }
}

describe('StartChange browser idempotency keys', () => {
  beforeEach(() => {
    window.localStorage.clear()
  })

  it('reuses one key across an unchanged approved reload and a running reconnect', () => {
    const mint = vi.fn(() => 'request-1')
    expect(startKeyFor(change(), undefined, mint)).toBe('request-1')
    expect(startKeyFor(change(), undefined, mint)).toBe('request-1')
    expect(startKeyFor(change({ status: 'running', updatedAt: 201 }), undefined, mint)).toBe('request-1')
    expect(mint).toHaveBeenCalledTimes(1)
  })

  it('retires the key on a terminal or non-startable state', () => {
    const mint = vi.fn(() => 'request-1')
    expect(startKeyFor(change(), undefined, mint)).toBe('request-1')
    reconcileStartKey(change({ status: 'completed', updatedAt: 201 }))
    expect(startKeyFor(change({ status: 'approved', updatedAt: 202 }), undefined, () => 'request-2')).toBe('request-2')

    reconcileStartKey(change({ status: 'pending', updatedAt: 203 }))
    expect(startKeyFor(change({ status: 'approved', updatedAt: 204 }), undefined, () => 'request-3')).toBe('request-3')
  })

  it('rotates when approved updatedAt changes, which may be a new plan or approval', () => {
    expect(startKeyFor(change(), undefined, () => 'old-plan-request')).toBe('old-plan-request')
    expect(startKeyFor(change({ updatedAt: 201 }), undefined, () => 'new-plan-request')).toBe('new-plan-request')
  })

  it('rotates on a plan change even when updatedAt has the same second', () => {
    expect(startKeyFor(change(), undefined, () => 'old-plan-request')).toBe('old-plan-request')
    const replanned = change({
      plan: { changeId: 'run-approved', targetHosts: ['web-2'], batches: [], impactSummary: 'new plan' },
    })
    expect(startKeyFor(replanned, undefined, () => 'new-plan-request')).toBe('new-plan-request')
  })

  it('recovers from malformed or incompatible stored records', () => {
    window.localStorage.setItem('levee.start.v1.run-approved', '{not json')
    expect(startKeyFor(change(), undefined, () => 'fresh-request')).toBe('fresh-request')
    const stored = JSON.parse(window.localStorage.getItem('levee.start.v1.run-approved') || '{}')
    expect(stored).toMatchObject({ version: 1, key: 'fresh-request', updatedAt: '200' })
  })

  it('fails before execution when durable browser storage is unavailable', () => {
    const storage = {
      getItem: vi.fn(() => { throw new Error('private mode') }),
      setItem: vi.fn(),
      removeItem: vi.fn(),
    } as unknown as Storage
    expect(() => startKeyFor(change(), storage, () => 'request-1')).toThrow('private mode')
    expect(storage.setItem).not.toHaveBeenCalled()
    expect(() => reconcileStartKey(change(), storage)).not.toThrow()
  })

  it('mints bounded unique keys with browser cryptography', () => {
    const first = mintStartKey()
    const second = mintStartKey()
    expect(first).not.toBe(second)
    expect(first.length).toBeLessThanOrEqual(128)
    expect(second.length).toBeLessThanOrEqual(128)
  })
})
