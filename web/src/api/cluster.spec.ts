// Unit tests for the cluster status API client wrapper. We verify the
// contract — systemApi.clusterStatus issues GET /system/cluster-status and
// returns the flat { nodes, summary, backend } shape — by swapping the
// transport at the axios adapter level, exactly like client.spec.ts does for
// the shared client. The backend handler (rest.go handleClusterStatus) writes
// a flat JSON object with no { data, error, meta } envelope.
import { AxiosError } from 'axios'
import { describe, expect, it, vi } from 'vitest'

const flatOk = {
	nodes: [{ id: 'n1', address: '10.0.0.1:9090', role: 'master', status: 'active', lastHeartbeat: '', joinedAt: '' }],
	summary: { counts: { pending: 1 }, nodeLoad: { n1: 1 }, totalActive: 1 },
	backend: 'postgres',
}

describe('systemApi.clusterStatus', () => {
	it('issues GET /system/cluster-status and returns the flat shape', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		const seen: { url?: string; method?: string } = {}
		client.axiosClient.defaults.adapter = async (config) => {
			seen.url = config.url
			seen.method = config.method
			return { data: flatOk, status: 200, statusText: 'OK', headers: {}, config }
		}

		const result = await mod.systemApi.clusterStatus()
		expect(seen.url).toBe('/system/cluster-status')
		expect(seen.method).toBe('get')
		expect(result.backend).toBe('postgres')
		expect(result.nodes).toHaveLength(1)
		expect(result.nodes?.[0]?.id).toBe('n1')
		expect(result.summary.totalActive).toBe(1)
	})

	it('propagates an HTTP error as a rejected promise', async () => {
		vi.resetModules()
		const mod = await import('./index')
		const client = await import('./client')

		client.axiosClient.defaults.adapter = async (config) => {
			const response = { data: { error: 'store unavailable' }, status: 500, statusText: 'Internal Server Error', headers: {}, config }
			throw new AxiosError('store unavailable', 'ERR_BAD_RESPONSE', config, {}, response as any)
		}

		await expect(mod.systemApi.clusterStatus()).rejects.toMatchObject({ code: 500, message: 'store unavailable' })
	})
})
