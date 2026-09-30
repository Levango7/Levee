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

	it('sendMessage issues POST /conversation/sessions/{id}/messages and unwraps {reply}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		const seen: { url?: string; body?: string } = {}
		client.axiosClient.defaults.adapter = async (config) => {
			seen.url = config.url
			seen.body = typeof config.data === 'string' ? config.data : undefined
			return {
				// Real backend wire shape: envelope {reply} with the engine's
				// nested action object ({type, payload}).
				data: { reply: { text: '建议摘要: test', action: { type: 'approve', payload: { recommendation_id: 'rec-1' } } } },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.sendMessage('s-1', 'op-1', '/help')
		expect(seen.url).toBe('/conversation/sessions/s-1/messages')
		expect(seen.body).toContain('/help')
		expect(result.text).toBe('建议摘要: test')
		expect(result.action_type).toBe('approve')
		expect(result.action_payload).toEqual({ recommendation_id: 'rec-1' })
		expect(result.session_id).toBe('s-1')
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

	it('getSession issues GET /conversation/sessions/{id} and unwraps {session}', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions/s-xyz')
			expect(config.method).toBe('get')
			expect(config.params).toEqual({ user_id: 'op-1' })
			return {
				data: { session: { id: 's-xyz', user_id: 'op-1', state: 'reviewing', messages: [], created_at: '', updated_at: '' } },
				status: 200, statusText: 'OK', headers: {}, config,
			}
		}

		const result = await mod.conversationApi.getSession('s-xyz', 'op-1')
		expect(result.state).toBe('reviewing')
	})

	it('closeSession issues DELETE /conversation/sessions/{id} with user_id param', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			expect(config.url).toBe('/conversation/sessions/s-xyz')
			expect(config.method).toBe('delete')
			expect(config.params).toEqual({ user_id: 'op-1' })
			return { data: null, status: 204, statusText: 'No Content', headers: {}, config }
		}

		await mod.conversationApi.closeSession('s-xyz', 'op-1')
	})
})
