// Unit tests for the conversation API client wrapper. Verifies the contract —
// newSession/sendMessage/getSession issue the correct HTTP verbs and paths —
// by swapping the transport at the axios adapter level (same pattern as
// cluster.spec.ts).
import { describe, expect, it, vi } from 'vitest'

describe('conversationApi', () => {
	it('newSession issues POST /conversation/sessions and unwraps {session}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions')
			expect(config.method).toBe('post')
			expect(config.data).toContain('"user_id":"op-1"')
			return {
				data: { session: { id: 's-1', user_id: 'op-1', state: 'idle', messages: [], created_at: '', updated_at: '' } },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.newSession('op-1')
		expect(result.id).toBe('s-1')
		expect(result.user_id).toBe('op-1')
	})

	it('sendMessage issues POST /conversation/sessions/{id}/messages', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		const seen: { url?: string; body?: string } = {}
		client.axiosClient.defaults.adapter = async (config) => {
			seen.url = config.url
			seen.body = typeof config.data === 'string' ? config.data : undefined
			return {
				data: { text: '建议摘要: test', action_type: 'none', session_id: 's-1' },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.sendMessage('s-1', 'op-1', '/help')
		expect(seen.url).toBe('/conversation/sessions/s-1/messages')
		expect(seen.body).toContain('/help')
		expect(result.text).toBe('建议摘要: test')
	})

	it('listSessions issues GET /conversation/sessions?user_id= and unwraps {sessions}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions')
			expect(config.method).toBe('get')
			return {
				data: { sessions: [{ id: 's-1', user_id: 'op-1', state: 'idle', messages: [], created_at: '', updated_at: '' }] },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.listSessions('op-1')
		expect(result).toHaveLength(1)
		expect(result?.[0]?.id).toBe('s-1')
	})

	it('getSession issues GET /conversation/sessions/{id}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions/s-xyz')
			expect(config.method).toBe('get')
			return {
				data: { id: 's-xyz', user_id: 'op-1', state: 'reviewing', messages: [], created_at: '', updated_at: '' },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.getSession('s-xyz')
		expect(result.state).toBe('reviewing')
	})

	it('closeSession issues DELETE /conversation/sessions/{id}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions/s-xyz')
			expect(config.method).toBe('delete')
			return { data: null, status: 204, statusText: 'No Content', headers: {}, config }
		}

		await mod.conversationApi.closeSession('s-xyz')
	})
})
