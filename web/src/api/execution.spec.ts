// Long-running change execution must not inherit the console's 30-second read
// timeout. The server runs Apply/Retry/Rollback synchronously and returns the
// terminal verdict; aborting the client wait early turns a valid long change
// into an ambiguous network error.
import { AxiosError } from 'axios'
import { describe, expect, it, vi } from 'vitest'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

async function freshApi() {
  vi.resetModules()
  const api = await import('./index')
  const client = await import('./client')
  return { api, client }
}

describe('long-running change execution requests', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  it('opts Apply, Retry and Rollback out of the 30s read deadline only', async () => {
    const { api, client } = await freshApi()
    expect(client.axiosClient.defaults.timeout).toBe(30_000)
    const seen: Array<{ url?: string; timeout?: number }> = []
    client.axiosClient.defaults.adapter = async (config) => {
      seen.push({ url: config.url, timeout: config.timeout })
      return { data: {}, status: 200, statusText: 'OK', headers: {}, config }
    }

    await api.changesApi.apply('run-1', {})
    await api.changesApi.retry('run-1', {})
    await api.changesApi.rollback('run-1', {})
    await api.changesApi.rollback('run-1', { dryRun: true })
    await api.changesApi.get('run-1')

    expect(seen).toEqual([
      { url: '/changes/run-1/apply', timeout: 0 },
      { url: '/changes/run-1/retry', timeout: 0 },
      { url: '/changes/run-1/rollback', timeout: 0 },
      { url: '/changes/run-1/rollback', timeout: 0 },
      { url: '/changes/run-1', timeout: 30_000 },
    ])
  })

  it('posts StartChange with its durable key, preserves duplicate and keeps the acceptance deadline bounded', async () => {
    const { api, client } = await freshApi()
    let request: { url?: string; timeout?: number; data?: unknown } = {}
    client.axiosClient.defaults.adapter = async (config) => {
      request = { url: config.url, timeout: config.timeout, data: config.data }
      return {
        data: { executionId: 'run-1', status: 'running', duplicate: true, message: 'replayed' },
        status: 200, statusText: 'OK', headers: {}, config,
      }
    }

    const response = await api.changesApi.start('run-1', {
      autoApprove: false,
      maxConcurrency: 2,
      idempotencyKey: 'request-1',
    })
    expect(request.url).toBe('/changes/run-1/start')
    expect(request.timeout).toBe(30_000)
    expect(JSON.parse(String(request.data))).toEqual({
      autoApprove: false,
      maxConcurrency: 2,
      idempotencyKey: 'request-1',
    })
    expect(response).toMatchObject({ executionId: 'run-1', status: 'running', duplicate: true })
  })

  it('calls a response-less execution outcome unknown, not a confirmed failure', async () => {
    const { api, client } = await freshApi()
    client.axiosClient.defaults.adapter = async (config) => {
      throw new AxiosError('socket closed', 'ERR_NETWORK', config, {}, undefined)
    }

    for (const invoke of [
      () => api.changesApi.start('run-1', { idempotencyKey: 'request-1' }),
      () => api.changesApi.apply('run-1', {}),
      () => api.changesApi.retry('run-1', {}),
      () => api.changesApi.rollback('run-1', {}),
    ]) {
      await expect(invoke()).rejects.toMatchObject({
        code: 0,
        message: '连接中断，执行结果未知：请先刷新变更状态，确认后再操作，勿直接重复提交',
      })
    }

    // A dry-run rollback is a read-only preview, not an execution whose
    // mutation outcome could be ambiguous.
    await expect(api.changesApi.rollback('run-1', { dryRun: true })).rejects.toEqual({
      code: 0,
      message: '网络异常：服务器未响应',
    })
  })

  it('treats a proxy 502/504 without an acceptance response as an unknown execution outcome', async () => {
    const { api, client } = await freshApi()
    for (const httpStatus of [502, 504]) {
      client.axiosClient.defaults.adapter = async (config) => {
        const response = { data: {}, status: httpStatus, statusText: '', headers: {}, config }
        throw new AxiosError('proxy disconnected', 'ERR_BAD_RESPONSE', config, {}, response)
      }
      await expect(api.changesApi.start('run-1', { idempotencyKey: 'request-1' })).rejects.toMatchObject({
        code: httpStatus,
        message: '连接中断，执行结果未知：请先刷新变更状态，确认后再操作，勿直接重复提交',
      })
    }
  })

  it('preserves the server verdict when an execution request is refused', async () => {
    const { api, client } = await freshApi()
    client.axiosClient.defaults.adapter = async (config) => {
      const response = { data: { error: 'approval required' }, status: 412, statusText: '', headers: {}, config }
      throw new AxiosError('failed precondition', 'ERR_BAD_REQUEST', config, {}, response)
    }

    await expect(api.changesApi.apply('run-1', {})).rejects.toMatchObject({
      code: 412,
      message: 'approval required',
    })
  })
})
