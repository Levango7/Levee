// identity.ts — who the console is acting as, read from the session token.
//
// The frontend has no user store: login (sso.ts) parks the server-issued
// LEVEE session token in localStorage and every request carries it as a
// bearer. That token is a JWT whose payload is internal/auth's
// SessionClaims (`sub`, `roles`, `login`, `tenant`), so the identity is
// already on the client — it was simply never read. Two places needed it
// and both hardcoded the answer: ApprovalView/MobileApprovalView default
// their approver field to the literal 'operator' and the topbar shows
// "operator" to everyone.
//
// Decoding the payload is NOT a trust decision: the server re-verifies the
// signature on every request (the frontend copy cannot and must not grant
// anything). This is attribution — the approver name lands in an audit row,
// and recording "operator" no matter who clicked is a defect in the audit
// trail, not a cosmetic one.
//
// The fallback exists because static-token deployments carry no session
// token at all (the bearer IS the identity source, and there is nothing to
// decode); those keep the historical literal.

import { getToken } from '@/api/client'

/** Best-effort current identity for display and audit attribution. */
export function currentSubject(fallback = 'operator'): string {
  const token = getToken()
  if (!token) return fallback
  const parts = token.split('.')
  if (parts.length !== 3) return fallback // static opaque token: no payload to read
  try {
    // base64url → JSON. `atob` is the browser's decoder; the payload is
    // UTF-8, hence the decodeURIComponent escape dance (btoa/atob are
    // Latin-1 by definition).
    // parts[1] is defined (length is 3), but the element type does not
    // narrow; the ?? keeps the type checker honest without a cast.
    const b64 = (parts[1] ?? '').replace(/-/g, '+').replace(/_/g, '/')
    const json = decodeURIComponent(
      Array.from(atob(b64))
        .map((c) => '%' + (c.charCodeAt(0) & 0xff).toString(16).padStart(2, '0'))
        .join(''),
    )
    const claims = JSON.parse(json) as { sub?: unknown }
    if (typeof claims.sub === 'string' && claims.sub !== '') return claims.sub
    return fallback
  } catch {
    // A malformed or non-JWT bearer must not break the view that asked;
    // attribution falls back exactly as it did when there was no token.
    return fallback
  }
}
