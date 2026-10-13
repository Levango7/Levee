import { describe, expect, it } from 'vitest'

import type { ApiError } from '@/types/levee'
import type { DeeplinkEvidence } from '@/api'
import { decidedByText, readDeeplinkEvidence } from './deeplinkEvidence'

function apiError(code: number, details: unknown): ApiError {
  return { code, message: 'deep link already consumed', details }
}

describe('readDeeplinkEvidence', () => {
  it('reads the evidence off a 409 body', () => {
    const ev: DeeplinkEvidence = {
      decided: true,
      decision: 'approved',
      run_id: 'run-42',
      decided_by: 'alice',
      decided_at: 1760349600,
    }
    expect(readDeeplinkEvidence(apiError(409, ev))).toEqual(ev)
  })

  it('returns null for a flat 401 (a token the server never issued)', () => {
    expect(readDeeplinkEvidence(apiError(401, { error: 'invalid or expired deeplink token' }))).toBeNull()
  })

  it('returns null for a 403 (this token may not approve)', () => {
    expect(readDeeplinkEvidence(apiError(403, { error: 'not an approver' }))).toBeNull()
  })

  it('returns null when the body has no decided flag', () => {
    expect(readDeeplinkEvidence(apiError(409, { decision: 'approved' }))).toBeNull()
  })

  it('returns null for the non-axios failure shapes this codebase throws', () => {
    expect(readDeeplinkEvidence(null)).toBeNull()
    expect(readDeeplinkEvidence(undefined)).toBeNull()
    expect(readDeeplinkEvidence('缺少变更 ID')).toBeNull()
    expect(readDeeplinkEvidence(new TypeError('Failed to fetch'))).toBeNull()
  })
})

describe('decidedByText', () => {
  it('names the approver for an approval', () => {
    expect(decidedByText({ decided: true, decision: 'approved', decided_by: 'alice' })).toBe('已由 alice 批准')
  })

  it('names the rejecter for a rejection', () => {
    expect(decidedByText({ decided: true, decision: 'rejected', decided_by: 'bob' })).toBe('已由 bob 驳回')
  })

  it('falls back to the decision alone for a legacy row with no approver', () => {
    expect(decidedByText({ decided: true, decision: 'approved' })).toBe('已批准')
    expect(decidedByText({ decided: true, decision: 'rejected' })).toBe('已驳回')
  })
})
