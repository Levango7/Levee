// Unit tests for the browser-side SSO helpers. The api client module is
// mocked (its own behaviour is covered by client.spec.ts); fetch and the
// Node WebCrypto implementation are stubbed globally so the OIDC/GitHub
// flows run end to end in jsdom. jsdom's window.location is unforgeable, so
// the redirect calls are no-ops there; we assert the persisted PKCE/state
// bookkeeping and the token-selection logic, which is where the security
// semantics live.
import { webcrypto } from 'node:crypto'
import { type Mock, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('./client', () => ({
  get: vi.fn(),
  post: vi.fn(),
  setToken: vi.fn(),
}))

import * as sso from './sso'
import { get, post, setToken } from './client'

const getMock = get as unknown as Mock
const postMock = post as unknown as Mock
const setTokenMock = setToken as unknown as Mock

// sessionStorage slots written by startSSOLogin / startGitHubLogin.
const VERIFIER = 'levee.sso.codeVerifier'
const STATE = 'levee.sso.state'
const REDIRECT = 'levee.sso.redirect'

let fetchMock: Mock

beforeEach(() => {
  localStorage.clear()
  sessionStorage.clear()
  getMock.mockReset()
  postMock.mockReset()
  setTokenMock.mockReset()
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  // jsdom's window.crypto has no subtle; the PKCE S256 challenge needs it.
  vi.stubGlobal('crypto', webcrypto)
})

function seedSSOSession(verifier = 'v1', state = 'st1', redirect = '/'): void {
  sessionStorage.setItem(VERIFIER, verifier)
  sessionStorage.setItem(STATE, state)
  sessionStorage.setItem(REDIRECT, redirect)
}

function tokenResponse(status: number, body: unknown, parseFails = false): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: parseFails ? async () => { throw new SyntaxError('bad json') } : async () => body,
  } as unknown as Response
}

describe('consumeSSORedirect', () => {
  it('returns and clears the stored in-app path', () => {
    sessionStorage.setItem(REDIRECT, '/changes/7')
    expect(sso.consumeSSORedirect()).toBe('/changes/7')
    expect(sessionStorage.getItem(REDIRECT)).toBeNull()
  })

  it('falls back when nothing is stored', () => {
    expect(sso.consumeSSORedirect()).toBe('/')
    expect(sso.consumeSSORedirect('/home')).toBe('/home')
  })

  it('rejects non-relative redirects (open-redirect guard)', () => {
    sessionStorage.setItem(REDIRECT, 'http://evil.example/steal')
    expect(sso.consumeSSORedirect('/safe')).toBe('/safe')
  })
})

describe('startSSOLogin', () => {
  it('rejects an incomplete descriptor', async () => {
    await expect(sso.startSSOLogin({ oidcEnabled: true }, '/')).rejects.toThrow('SSO 未启用或配置不完整')
  })

  it('persists a fresh PKCE verifier and state before redirecting', async () => {
    await sso.startSSOLogin(
      { oidcEnabled: true, authorizeUrl: 'http://idp/auth', clientId: 'cid' },
      '/changes',
    )
    const verifier = sessionStorage.getItem(VERIFIER)
    const state = sessionStorage.getItem(STATE)
    expect(verifier).toBeTruthy()
    expect(verifier!.length).toBeGreaterThan(40)
    expect(state).toBeTruthy()
    expect(verifier).not.toBe(state)
    expect(sessionStorage.getItem(REDIRECT)).toBe('/changes')
  })
})

describe('startGitHubLogin', () => {
  it('rejects without a client id', () => {
    expect(() => sso.startGitHubLogin({ oidcEnabled: false }, '/')).toThrow('GitHub 登录未启用或配置不完整')
  })

  it('persists state and the in-app redirect', () => {
    sso.startGitHubLogin({ oidcEnabled: false, githubClientId: 'gh-cid' }, '/dash')
    expect(sessionStorage.getItem(STATE)).toBeTruthy()
    expect(sessionStorage.getItem(REDIRECT)).toBe('/dash')
    expect(sessionStorage.getItem(VERIFIER)).toBeNull() // PKCE does not apply
  })
})

describe('completeSSOLogin', () => {
  it('fails without a stored session', async () => {
    await expect(sso.completeSSOLogin('code', 'st1')).rejects.toThrow('SSO 会话已过期，请重新登录')
  })

  it('fails on state mismatch (CSRF guard)', async () => {
    seedSSOSession()
    await expect(sso.completeSSOLogin('code', 'other')).rejects.toThrow('状态校验失败')
    expect(setTokenMock).not.toHaveBeenCalled()
  })

  it('fails when the descriptor is incomplete', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true })
    await expect(sso.completeSSOLogin('code', 'st1')).rejects.toThrow('SSO 配置不完整，请联系管理员')
  })

  it('exchanges the code with the verifier and stores a JWT access token', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true, tokenUrl: 'http://idp/token', clientId: 'cid' })
    fetchMock.mockResolvedValue(tokenResponse(200, { access_token: 'a.b.c' }))

    await sso.completeSSOLogin('the-code', 'st1')

    const [url, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit]
    expect(url).toBe('http://idp/token')
    expect(init.method).toBe('POST')
    const body = String(init.body)
    expect(body).toContain('grant_type=authorization_code')
    expect(body).toContain('code=the-code')
    expect(body).toContain('code_verifier=v1')
    expect(body).toContain('client_id=cid')
    expect(body).toContain('redirect_uri=http%3A%2F%2Flocalhost%2Flogin%2Fcallback')
    expect(setTokenMock).toHaveBeenCalledWith('a.b.c')
    // One-shot credentials are wiped after a successful exchange.
    expect(sessionStorage.getItem(VERIFIER)).toBeNull()
    expect(sessionStorage.getItem(STATE)).toBeNull()
    expect(sessionStorage.getItem(REDIRECT)).toBeNull()
  })

  it('falls back to the id_token when the access token is not a JWT', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true, tokenUrl: 'http://idp/token', clientId: 'cid' })
    fetchMock.mockResolvedValue(tokenResponse(200, { access_token: 'opaque', id_token: 'i.d.t' }))
    await sso.completeSSOLogin('the-code', 'st1')
    expect(setTokenMock).toHaveBeenCalledWith('i.d.t')
  })

  it('rejects when the IdP returns no usable token', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true, tokenUrl: 'http://idp/token', clientId: 'cid' })
    fetchMock.mockResolvedValue(tokenResponse(200, { access_token: 'opaque' }))
    await expect(sso.completeSSOLogin('the-code', 'st1')).rejects.toThrow('IdP 未返回可用的 JWT 令牌')
  })

  it('surfaces error_description from a failed exchange', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true, tokenUrl: 'http://idp/token', clientId: 'cid' })
    fetchMock.mockResolvedValue(tokenResponse(400, { error: 'invalid_grant', error_description: 'code expired' }))
    await expect(sso.completeSSOLogin('the-code', 'st1')).rejects.toThrow('code expired')
  })

  it('falls back to the HTTP status when the error body cannot be parsed', async () => {
    seedSSOSession()
    getMock.mockResolvedValue({ oidcEnabled: true, tokenUrl: 'http://idp/token', clientId: 'cid' })
    fetchMock.mockResolvedValue(tokenResponse(502, null, true))
    await expect(sso.completeSSOLogin('the-code', 'st1')).rejects.toThrow('令牌交换失败（HTTP 502）')
  })
})

describe('completeGitHubLogin', () => {
  it('fails without a stored state', async () => {
    await expect(sso.completeGitHubLogin('code', 'st1')).rejects.toThrow('SSO 会话已过期，请重新登录')
  })

  it('fails on state mismatch (CSRF guard)', async () => {
    seedSSOSession()
    await expect(sso.completeGitHubLogin('code', 'other')).rejects.toThrow('状态校验失败')
  })

  it('stores the session token returned by the gateway', async () => {
    seedSSOSession('unused-verifier', 'st1', '/home')
    postMock.mockResolvedValue({ token: 'sess-tok', subject: 'bob' })
    await sso.completeGitHubLogin('the-code', 'st1')
    expect(postMock).toHaveBeenCalledWith('/auth/github', { code: 'the-code', state: 'st1' })
    expect(setTokenMock).toHaveBeenCalledWith('sess-tok')
    expect(sessionStorage.getItem(STATE)).toBeNull()
    expect(sessionStorage.getItem(REDIRECT)).toBeNull()
  })

  it('rejects when the gateway omits the session token', async () => {
    seedSSOSession()
    postMock.mockResolvedValue({ subject: 'bob' })
    await expect(sso.completeGitHubLogin('code', 'st1')).rejects.toThrow('服务端未返回会话令牌')
  })
})
