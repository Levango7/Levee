// Single owner for the three-way classification of a region's load outcome.
//
// The roadmap asks every list / detail / log region to distinguish EMPTY,
// FAILED and NOT-PERMITTED instead of the old behaviour, where a failed fetch
// only raised a toast and an exhausted filter and a broken backend both looked
// like "zero rows". Three answers exist because three different actions
// follow: empty → offer the next step (clear the filter, create the first
// thing), error → offer a retry, forbidden → offer a re-login with a role
// that CAN see this.
//
// The only signal the server gives for the third tier is HTTP 403: the REST
// gateway maps grpc PermissionDenied to StatusForbidden with the handler's
// message verbatim (internal/grpc/rest.go), so the frontend never has to
// guess which role was missing — it quotes the server. Everything else that
// goes wrong (500, timeout, offline) is the same answer "retry", so the
// remaining codes deliberately collapse into 'error' rather than each getting
// its own face.
import type { ApiError } from '@/types/levee'

export type LoadTier = 'ok' | 'empty' | 'error' | 'forbidden'

/**
 * The three terminal answers a region can show. `StateBlock` accepts only
 * these: 'ok' is not a state to render, so it is not a prop value either.
 */
export type StateTier = Exclude<LoadTier, 'ok'>

/**
 * Maps a caught fetch failure to the tier the region should show.
 *
 * A 403 is 'forbidden' (the request was well-formed and understood; the
 * principal may not do it). A 401 never reaches this function in practice —
 * client.ts clears the token and redirects to /login on the way out — but if
 * it ever did, it is treated as forbidden rather than as an error to retry,
 * because re-sending an expired session cannot succeed.
 */
export function classifyError(err: unknown): 'error' | 'forbidden' {
  const code = (err as Partial<ApiError> | null | undefined)?.code
  if (code === 403 || code === 401) return 'forbidden'
  return 'error'
}

/**
 * Reads the operator-facing message off a failure, falling back when the
 * caught value is not an ApiError at all (a thrown string — which is a real
 * pattern in this codebase, e.g. MobileApprovalView's "缺少变更 ID"
 * precondition — a TypeError from a programming mistake, null). Never returns
 * an empty string: an error state with no text is the bug this file exists to
 * remove.
 */
export function errorMessage(err: unknown, fallback: string): string {
  if (typeof err === 'string' && err.trim() !== '') return err
  const msg = (err as Partial<ApiError> | null | undefined)?.message
  return typeof msg === 'string' && msg.trim() !== '' ? msg : fallback
}
