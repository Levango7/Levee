// Unit tests for the ad-hoc gate verification API client wrapper.
//
// /gates/verify is a hand-written REST endpoint (internal/grpc/rest_gate.go)
// whose JSON struct tags are snake_case — unlike the protojson camelCase the
// rest of this client translates. The fixture below is the SERVER's spelling,
// and the assertions pin the two fields a camelCase reading would silently
// turn into `undefined` (latency_ms / ran_at). That drift class is exactly
// what the cluster.spec.ts header documents paying for once already.
import { AxiosError } from 'axios'
import { describe, expect, it, vi } from 'vitest'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

const verdictOk = {
	gate: 'ad-hoc',
	type: 'cmd',
	target: 'web-1',
	passed: true,
	message: 'exit code 0 as expected',
	details: { stdout: 'active' },
	latency_ms: 42,
	ran_at: '2026-10-28T09:00:00Z',
}

describe('gatesApi.verify', { timeout: COLD_IMPORT_TIMEOUT }, () => {
	it('posts the check to /gates/verify and returns the snake_case verdict', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		const seen: { url?: string; method?: string; body?: unknown } = {}
		client.axiosClient.defaults.adapter = async (config) => {
			seen.url = config.url
			seen.method = config.method
			seen.body = typeof config.data === 'string' ? JSON.parse(config.data) : config.data
			return { data: verdictOk, status: 200, statusText: 'OK', headers: {}, config }
		}

		const result = await mod.gatesApi.verify({
			type: 'cmd',
			target: 'web-1',
			params: { cmd: 'systemctl is-active nginx' },
		})

		expect(seen.url).toBe('/gates/verify')
		expect(seen.method).toBe('post')
		expect(seen.body).toEqual({
			type: 'cmd',
			target: 'web-1',
			params: { cmd: 'systemctl is-active nginx' },
		})
		expect(result.passed).toBe(true)
		expect(result.latency_ms).toBe(42)
		expect(result.ran_at).toBe('2026-10-28T09:00:00Z')
	})

	it('propagates a 400 (a check the server refuses to execute) as a rejected promise', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			const response = {
				data: { error: 'unknown check type "wat"' },
				status: 400,
				statusText: 'Bad Request',
				headers: {},
				config,
			}
			throw new AxiosError('unknown check type "wat"', 'ERR_BAD_REQUEST', config, {}, response as never)
		}

		await expect(
			mod.gatesApi.verify({ type: 'cmd', params: { cmd: 'true' } }),
		).rejects.toMatchObject({ code: 400 })
	})
})
