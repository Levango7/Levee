// identity.spec.ts — who the console is acting as.
//
// The approver field on an approval decision lands in an audit row, so
// reading it wrong is an audit-trail defect, not a cosmetic one: the old
// hardcoded 'operator' attributed every mobile decision and every
// desktop default to a name nobody chose. These tests pin the decoding
// and — more importantly — the failure posture: a missing or malformed
// token must degrade to the historical fallback rather than break the
// view that asked.
import { describe, expect, it } from 'vitest'
import { currentSubject } from './identity'

// b64url payloads for the shapes the session token and the static bearer
// can take. The server verify path is NOT under test (it cannot be, from
// here): these cover decoding and fallbacks only.
function tokenWith(payload: Record<string, unknown>): string {
  const body = btoa(JSON.stringify(payload)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
  return `header.${body}.signature`
}

describe('currentSubject', () => {
  it('reads the sub claim from a session token', () => {
    localStorage.setItem('levee.token', tokenWith({ sub: 'alice', roles: ['operator'] }))
    expect(currentSubject()).toBe('alice')
  })

  it('falls back when there is no token (static bearer deployments)', () => {
    localStorage.removeItem('levee.token')
    expect(currentSubject()).toBe('operator')
  })

  it('falls back for an opaque non-JWT bearer instead of guessing', () => {
    localStorage.setItem('levee.token', 'a-static-opaque-bearer-value')
    expect(currentSubject()).toBe('operator')
  })

  it('falls back when the payload is not decodable JSON', () => {
    localStorage.setItem('levee.token', 'header.!!!not-base64!!!.signature')
    expect(currentSubject()).toBe('operator')
  })

  it('falls back when the payload carries no usable sub', () => {
    localStorage.setItem('levee.token', tokenWith({ roles: ['operator'] }))
    expect(currentSubject()).toBe('operator')
  })

  it('honours an explicit fallback name', () => {
    localStorage.removeItem('levee.token')
    expect(currentSubject('system')).toBe('system')
  })
})
