// SSO (OpenID Connect) login helpers for the browser SPA.
//
// Flow (authorization code + PKCE, public client — no client secret):
//   1. LoginView fetches the auth descriptor from GET /system/auth-info
//      (public, no bearer required) to learn whether SSO is offered and
//      where the IdP's authorization/token endpoints are.
//   2. startSSOLogin generates a PKCE code_verifier/challenge plus a
//      anti-CSRF state, persists them in sessionStorage and redirects the
//      browser to the IdP's authorization endpoint.
//   3. The IdP redirects back to /login/callback?code=...&state=...
//   4. completeSSOLogin exchanges the code at the IdP's token endpoint
//      directly from the browser (requires the IdP to allow CORS for this
//      origin — standard for SPA clients) and stores the resulting token.
//
// The stored token is the access token when it is a JWT (three dot-separated
// segments, verifiable by the backend); otherwise the id_token is used,
// which is always a JWT the OIDC verifier accepts. (GitHub logins store a
// LEVEE session token returned by the gateway instead — see below.)
import { get, post, setToken } from './client'

const SSO_VERIFIER_KEY = 'levee.sso.codeVerifier'
const SSO_STATE_KEY = 'levee.sso.state'
const SSO_REDIRECT_KEY = 'levee.sso.redirect'

// AuthInfo mirrors the public GET /system/auth-info response.
export interface AuthInfo {
  oidcEnabled: boolean
  issuerUrl?: string
  clientId?: string
  authorizeUrl?: string
  tokenUrl?: string
  githubEnabled?: boolean
  githubClientId?: string
  githubOrg?: string
}

export function fetchAuthInfo(): Promise<AuthInfo> {
  return get<AuthInfo>('/system/auth-info')
}

function base64UrlEncode(bytes: Uint8Array): string {
  let bin = ''
  bytes.forEach((b) => (bin += String.fromCharCode(b)))
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function randomString(length = 64): string {
  const bytes = new Uint8Array(length)
  crypto.getRandomValues(bytes)
  return base64UrlEncode(bytes)
}

async function s256Challenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier))
  return base64UrlEncode(new Uint8Array(digest))
}

// startSSOLogin redirects the browser to the IdP's authorization endpoint.
// `redirect` is the in-app path to return to after the round-trip.
export async function startSSOLogin(authInfo: AuthInfo, redirect: string): Promise<void> {
  if (!authInfo.authorizeUrl || !authInfo.clientId) {
    throw new Error('SSO 未启用或配置不完整')
  }
  const verifier = randomString(64)
  const state = randomString(16)
  sessionStorage.setItem(SSO_VERIFIER_KEY, verifier)
  sessionStorage.setItem(SSO_STATE_KEY, state)
  sessionStorage.setItem(SSO_REDIRECT_KEY, redirect)

  const redirectUri = `${window.location.origin}/login/callback`
  const challenge = await s256Challenge(verifier)
  const url = new URL(authInfo.authorizeUrl)
  url.searchParams.set('response_type', 'code')
  url.searchParams.set('client_id', authInfo.clientId)
  url.searchParams.set('redirect_uri', redirectUri)
  url.searchParams.set('scope', 'openid profile email')
  url.searchParams.set('state', state)
  url.searchParams.set('code_challenge', challenge)
  url.searchParams.set('code_challenge_method', 'S256')
  window.location.assign(url.toString())
}

interface TokenResponse {
  access_token?: string
  id_token?: string
  token_type?: string
  error?: string
  error_description?: string
}

// LooksLikeJWT reports whether token has the three-segment JWT shape; the
// backend uses the same heuristic to pick the verification path.
function looksLikeJWT(token: string): boolean {
  return token.split('.').length === 3
}

// completeSSOLogin exchanges the authorization code for tokens and stores
// the one the backend can verify. The post-login in-app redirect is handled
// separately via consumeSSORedirect. Throws with a user-presentable message
// on any failure.
export async function completeSSOLogin(code: string, state: string): Promise<void> {
  const verifier = sessionStorage.getItem(SSO_VERIFIER_KEY)
  const expectedState = sessionStorage.getItem(SSO_STATE_KEY)
  if (!verifier || !expectedState) {
    throw new Error('SSO 会话已过期，请重新登录')
  }
  if (state !== expectedState) {
    throw new Error('SSO 状态校验失败（可能的 CSRF），请重新登录')
  }
  const authInfo = await fetchAuthInfo()
  if (!authInfo.tokenUrl || !authInfo.clientId) {
    throw new Error('SSO 配置不完整，请联系管理员')
  }

  const body = new URLSearchParams({
    grant_type: 'authorization_code',
    code,
    redirect_uri: `${window.location.origin}/login/callback`,
    client_id: authInfo.clientId,
    code_verifier: verifier,
  })
  const resp = await fetch(authInfo.tokenUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: body.toString(),
  })
  const data = (await resp.json().catch(() => ({}))) as TokenResponse
  if (!resp.ok || data.error) {
    throw new Error(data.error_description || data.error || `令牌交换失败（HTTP ${resp.status}）`)
  }

  const token = data.access_token && looksLikeJWT(data.access_token)
    ? data.access_token
    : data.id_token
  if (!token) {
    throw new Error('IdP 未返回可用的 JWT 令牌')
  }
  setToken(token)
  sessionStorage.removeItem(SSO_VERIFIER_KEY)
  sessionStorage.removeItem(SSO_STATE_KEY)
  sessionStorage.removeItem(SSO_REDIRECT_KEY)
}

// consumeSSORedirect returns and clears the in-app path stashed by
// startSSOLogin, falling back to `fallback`.
export function consumeSSORedirect(fallback = '/'): string {
  const redirect = sessionStorage.getItem(SSO_REDIRECT_KEY)
  sessionStorage.removeItem(SSO_REDIRECT_KEY)
  return redirect && redirect.startsWith('/') ? redirect : fallback
}

// --- GitHub OAuth flow -------------------------------------------------------
//
// GitHub's token endpoint does not serve browser CORS, so unlike the OIDC
// flow the code exchange runs server-side: the browser POSTs the code to
// the LEVEE gateway (POST /auth/github) and receives a LEVEE session token.
// PKCE does not apply to GitHub's flow; the CSRF state stays the same.

const GITHUB_AUTHORIZE_URL = 'https://github.com/login/oauth/authorize'

// startGitHubLogin redirects the browser to GitHub's authorization page.
// The state nonce is shared with the OIDC flow (same sessionStorage slot).
export function startGitHubLogin(authInfo: AuthInfo, redirect: string): void {
  if (!authInfo.githubClientId) {
    throw new Error('GitHub 登录未启用或配置不完整')
  }
  const state = randomString(16)
  sessionStorage.setItem(SSO_STATE_KEY, state)
  sessionStorage.setItem(SSO_REDIRECT_KEY, redirect)

  const url = new URL(GITHUB_AUTHORIZE_URL)
  url.searchParams.set('client_id', authInfo.githubClientId)
  url.searchParams.set('redirect_uri', `${window.location.origin}/login/callback`)
  url.searchParams.set('scope', 'read:user read:org')
  url.searchParams.set('state', state)
  window.location.assign(url.toString())
}

// completeGitHubLogin exchanges the authorization code server-side and
// stores the returned LEVEE session token. The gateway enforces the org
// restriction and team role mapping; failures surface as user-presentable
// errors here.
export async function completeGitHubLogin(code: string, state: string): Promise<void> {
  const expectedState = sessionStorage.getItem(SSO_STATE_KEY)
  if (!expectedState) {
    throw new Error('SSO 会话已过期，请重新登录')
  }
  if (state !== expectedState) {
    throw new Error('SSO 状态校验失败（可能的 CSRF），请重新登录')
  }
  const login = await post<{ token: string; subject: string; roles?: string[] }>(
    '/auth/github',
    { code, state },
  )
  if (!login.token) {
    throw new Error('服务端未返回会话令牌')
  }
  setToken(login.token)
  sessionStorage.removeItem(SSO_STATE_KEY)
  sessionStorage.removeItem(SSO_REDIRECT_KEY)
}
