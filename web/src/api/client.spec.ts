// Unit tests for the shared api client: token storage tri-state, the Bearer
// request interceptor, and the AxiosError -> ApiError normalisation. The
// transport is swapped at the axios adapter level, so interceptors, baseURL
// and unwrapping run for real without any network.
//
// Note: jsdom makes window.location unforgeable, so the 401 redirect itself
// cannot be observed here; we assert its visible side effects instead (the
// token is cleared before the redirect attempt, the normalised 401 payload).
import { AxiosError, type AxiosAdapter, type InternalAxiosRequestConfig } from 'axios'
import { beforeEach, describe, expect, it, vi } from 'vitest'

type ClientModule = typeof import('./client')

// Fresh module instance per test: resets the `redirectingToLogin` singleton
// and gives each test its own axios instance (interceptors are registered at
// module load).
async function freshClient(): Promise<ClientModule> {
  vi.resetModules()
  return await import('./client')
}

function okAdapter(payload: unknown, seen?: { current?: InternalAxiosRequestConfig }): AxiosAdapter {
  return async (config) => {
    if (seen) seen.current = config
    return { data: payload, status: 200, statusText: 'OK', headers: {}, config }
  }
}

function responseErrorAdapter(status: number, data: unknown, message = ''): AxiosAdapter {
  return async (config) => {
    const response = { data, status, statusText: '', headers: {}, config }
    throw new AxiosError(message || `Request failed with status code ${status}`, 'ERR_BAD_RESPONSE', config, {}, response)
  }
}

describe('token storage', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('defaults to an empty token', async () => {
    const m = await freshClient()
    expect(m.getToken()).toBe('')
  })

  it('setToken stores and empty string clears', async () => {
    const m = await freshClient()
    m.setToken('t-1')
    expect(m.getToken()).toBe('t-1')
    expect(localStorage.getItem('levee.token')).toBe('t-1')
    m.setToken('')
    expect(m.getToken()).toBe('')
  })

  it('clearToken removes the stored value', async () => {
    const m = await freshClient()
    m.setToken('t-2')
    m.clearToken()
    expect(localStorage.getItem('levee.token')).toBeNull()
    expect(m.getToken()).toBe('')
  })
})

describe('request pipeline', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('attaches the Bearer header when a token is stored', async () => {
    const m = await freshClient()
    const seen: { current?: InternalAxiosRequestConfig } = {}
    m.setToken('t-7')
    m.axiosClient.defaults.adapter = okAdapter({ ok: true }, seen)
    const data = await m.get<{ ok: boolean }>('/changes')
    expect(data).toEqual({ ok: true })
    expect((seen.current?.headers as unknown as Record<string, unknown>).Authorization).toBe('Bearer t-7')
    expect(seen.current?.baseURL).toBe('/api/v1')
    expect(seen.current?.url).toBe('/changes')
  })

  it('omits the Authorization header without a token', async () => {
    const m = await freshClient()
    const seen: { current?: InternalAxiosRequestConfig } = {}
    m.axiosClient.defaults.adapter = okAdapter(null, seen)
    await m.post('/changes', { label: 'x' })
    const auth = (seen.current?.headers as unknown as Record<string, unknown>).Authorization
    expect(auth).toBeUndefined()
  })
})

describe('error normalisation', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('maps a server {"error": ...} body to code/message/details', async () => {
    const m = await freshClient()
    m.axiosClient.defaults.adapter = responseErrorAdapter(500, { error: 'boom' })
    await expect(m.get('/x')).rejects.toMatchObject({ code: 500, message: 'boom' })
  })

  it('falls back to {message} then the HTTP status when error is absent', async () => {
    const m = await freshClient()
    m.axiosClient.defaults.adapter = responseErrorAdapter(502, { message: 'bad gateway' })
    await expect(m.get('/x')).rejects.toMatchObject({ code: 502, message: 'bad gateway' })

    const m2 = await freshClient()
    m2.axiosClient.defaults.adapter = responseErrorAdapter(503, {})
    await expect(m2.get('/x')).rejects.toMatchObject({ code: 503, message: 'Request failed with status code 503' })
  })

  it('maps a response-less request failure to code 0 (network)', async () => {
    const m = await freshClient()
    m.axiosClient.defaults.adapter = async (config) => {
      throw new AxiosError('timeout of 30000ms exceeded', 'ECONNABORTED', config, {}, undefined)
    }
    await expect(m.get('/x')).rejects.toEqual({ code: 0, message: '网络异常：服务器未响应' })
  })

  it('maps a pre-request failure to code -1', async () => {
    const m = await freshClient()
    m.axiosClient.defaults.adapter = async () => {
      throw new AxiosError('boom')
    }
    await expect(m.get('/x')).rejects.toEqual({ code: -1, message: 'boom' })
  })
})

describe('401 handling', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('clears the token and returns the unified session-expired message', async () => {
    const m = await freshClient()
    m.setToken('stale')
    m.axiosClient.defaults.adapter = responseErrorAdapter(401, { error: 'unauthenticated' })
    await expect(m.get('/x')).rejects.toMatchObject({
      code: 401,
      message: '登录已过期，请重新登录',
      details: { error: 'unauthenticated' },
    })
    expect(m.getToken()).toBe('')
  })

  it('leaves other statuses’ tokens untouched', async () => {
    const m = await freshClient()
    m.setToken('keep-me')
    m.axiosClient.defaults.adapter = responseErrorAdapter(403, { error: 'forbidden' })
    await expect(m.get('/x')).rejects.toMatchObject({ code: 403, message: 'forbidden' })
    expect(m.getToken()).toBe('keep-me')
  })
})
