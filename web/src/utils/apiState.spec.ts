import { describe, expect, it } from 'vitest'

import { classifyError, errorMessage } from './apiState'

// Unit tests for the region-state classification. The roadmap's row asks every
// list / detail / log region to answer EMPTY / FAILED / NOT-PERMITTED instead
// of a toast over an unchanged table; this is the decision function all of them
// share, and the distinction it makes is load-bearing:

//   - 403 is the ONLY "you may not" signal the REST gateway gives
//     (internal/grpc/rest.go maps grpc PermissionDenied with the handler's
//     message verbatim), so the region can quote the server instead of
//     inventing a policy explanation.
//   - everything else collapses to 'error' (retry), because a 500, a timeout
//     and an offline tab all get the same next step. One face per ACTION, not
//     one face per HTTP code.

describe('classifyError', () => {
  it('reads a forbidden read as its own tier, not as a retry', () => {
    expect(classifyError({ code: 403, message: 'session belongs to another user' })).toBe('forbidden')
    // A 401 cannot be retried either: the session is gone, the answer is a
    // re-login, which is the same next step as a forbidden read.
    expect(classifyError({ code: 401, message: '登录已过期，请重新登录' })).toBe('forbidden')
  })

  it('collapses every other failure into the retryable tier', () => {
    for (const code of [0, -1, 400, 404, 409, 412, 429, 500, 502, 504]) {
      expect(classifyError({ code, message: 'x' }), `code ${code}`).toBe('error')
    }
  })

  it('defaults a value that is not an ApiError at all', () => {
    // A thrown string, a TypeError from a programming mistake, null: none of
    // them carry an HTTP status, so none of them may claim 'forbidden'.
    expect(classifyError('boom')).toBe('error')
    expect(classifyError(new Error('boom'))).toBe('error')
    expect(classifyError(null)).toBe('error')
    expect(classifyError(undefined)).toBe('error')
    expect(classifyError({})).toBe('error')
  })
})

describe('errorMessage', () => {
  it('passes a thrown string through, it is already operator-facing', () => {
    // MobileApprovalView's "缺少变更 ID" precondition is exactly this shape.
    expect(errorMessage('缺少变更 ID', 'fallback')).toBe('缺少变更 ID')
  })

  it('prefers the server message over our own wording', () => {
    expect(errorMessage({ code: 403, message: 'unauthorized approver: mallory' }, 'fallback')).toBe(
      'unauthorized approver: mallory',
    )
  })

  it('never returns an empty region message', () => {
    // An error state with no text is the bug this file exists to remove: the
    // operator would see "加载失败" with nothing under it.
    expect(errorMessage({ code: 500, message: '' }, 'fallback')).toBe('fallback')
    expect(errorMessage({ code: 500, message: '   ' }, 'fallback')).toBe('fallback')
    expect(errorMessage({ code: 500 }, 'fallback')).toBe('fallback')
    expect(errorMessage(null, 'fallback')).toBe('fallback')
    expect(errorMessage(undefined, 'fallback')).toBe('fallback')
  })
})
