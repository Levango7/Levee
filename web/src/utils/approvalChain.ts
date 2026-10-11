// Summary of an approval chain for the approval centre's card. Pure so it can
// be unit-tested without mounting the view (this repo has no @vue/test-utils).
//
// The card exists to answer one question the row alone cannot: why is a vote
// not landing? The answers it must not hide are the quorum shortfall, who has
// not decided, whether the initiator is barred from deciding, and whether the
// chain is bound to a plan version at all (a legacy chain still settles the run
// regardless of plan, which the card must state rather than render a blank).
import type { ApprovalChain } from '@/api'

export interface ApprovalSummary extends ApprovalChain {
  /** How many approvers have cast a decision. */
  decided: number
  /** Approvers with no decision yet, in declaration order. */
  pending: string[]
}

/** approvalSummary keeps the wire chain and adds the two derived facts the
 *  card shows. Deriving them in the UI is safe: the values come from the
 *  response, not from a second read of the store, and the same set difference
 *  happens server-side for the quorum gate. */
export function approvalSummary(chains: ApprovalChain[]): ApprovalSummary[] {
  return chains.map((c) => {
    const decided = c.decisions.length
    const done = new Set(c.decisions.map((d) => d.approver))
    return {
      ...c,
      decided,
      pending: c.approvers.filter((a) => !done.has(a)),
    }
  })
}

// Re-exported so the view does not import the wire type from two places: the
// chain card's props are the API's own shape, plus the two derived facts.
export type { ApprovalChain } from '@/api'
