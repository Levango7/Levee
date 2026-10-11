import { describe, expect, it } from 'vitest'

import type { ApprovalChain } from '@/api'
import { approvalSummary } from './approvalChain'

const chain = (patch: Partial<ApprovalChain>): ApprovalChain => ({
  id: 'ap-1',
  changeId: 'run-1',
  level: 'standard',
  status: 'pending',
  approvers: ['bob', 'carol', 'dave'],
  minApprovers: 2,
  decisions: [],
  expiresAt: 0,
  planHash: '',
  legacy: true,
  initiator: 'alice',
  excludeInitiator: true,
  stale: false,
  ...patch,
})

describe('approvalSummary', () => {
  it('counts the decided votes and names who has not voted', () => {
    const out = approvalSummary([
      chain({
        decisions: [
          { approver: 'bob', action: 'approve', reason: '', decidedAt: 100 },
          { approver: 'carol', action: 'reject', reason: '窗口不合适', decidedAt: 200 },
        ],
      }),
    ])
    expect(out).toHaveLength(1)
    expect(out[0]?.decided).toBe(2)
    expect(out[0]?.pending).toEqual(['dave'])
  })

  it('keeps the wire facts the card shows, including legacy', () => {
    const c = chain({ planHash: 'plan-1', legacy: false, level: 'high', minApprovers: 3 })
    const out = approvalSummary([c])[0]
    expect(out?.legacy).toBe(false)
    expect(out?.planHash).toBe('plan-1')
    expect(out?.level).toBe('high')
    expect(out?.minApprovers).toBe(3)
    expect(out?.excludeInitiator).toBe(true)
    expect(out?.pending).toEqual(['bob', 'carol', 'dave'])
  })

  it('does not invent an order for the pending set', () => {
    const out = approvalSummary([
      chain({
        approvers: ['dave', 'bob', 'carol'],
        decisions: [{ approver: 'bob', action: 'approve', reason: '', decidedAt: 1 }],
      }),
    ])[0]
    expect(out?.pending).toEqual(['dave', 'carol'])
  })

  it('handles an empty chain list', () => {
    expect(approvalSummary([])).toEqual([])
  })
})
