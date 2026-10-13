// Reads the "already decided" evidence off a failed one-tap deep-link call.
//
// A one-time token is spent by the tap that settled it, so the SECOND tap
// always fails — and the operator's real question at that point is whether
// their change went through, not whether the token was well-formed. The
// gateway answers that case with HTTP 409 and a structured body
// (see internal/grpc/rest.go: writeDeeplinkFailure): the decided flag is
// what distinguishes it from every other failure, which is why it — and not
// the status code alone — gates the evidence here.
//
// axios rejects the 409, and client.ts's normalizeError keeps the whole
// response body under `details`, so the evidence survives into the catch.
// A flat 401 (a token the server never issued) carries no such body: it must
// stay a plain error, because that response deliberately discloses nothing.
import type { ApiError } from '@/types/levee'
import type { DeeplinkEvidence } from '@/api'

/**
 * Returns the evidence when the failure means "this change has already been
 * decided", and null for every other failure (flat 401, 403, network, a
 * thrown precondition string). Null means: show the ordinary error.
 */
export function readDeeplinkEvidence(err: unknown): DeeplinkEvidence | null {
  const details = (err as Partial<ApiError> | null | undefined)?.details as
    | Partial<DeeplinkEvidence>
    | undefined
  return details?.decided === true ? (details as DeeplinkEvidence) : null
}

/**
 * One line for the operator: who decided the change and when, or a bare
 * decision when the record carries no approver (a legacy row). Never empty:
 * a decided notice with no sentence is the bug this file exists to remove.
 */
export function decidedByText(ev: DeeplinkEvidence): string {
  const who = ev.decided_by?.trim()
  const verb = ev.decision === 'rejected' ? '驳回' : '批准'
  return who ? `已由 ${who} ${verb}` : `已${verb}`
}
