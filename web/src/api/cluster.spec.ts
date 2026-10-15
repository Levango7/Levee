// Unit tests for the cluster status API client wrapper. We verify the
// contract — systemApi.clusterStatus issues GET /system/cluster-status and
// returns the flat { nodes, summary, backend } shape — by swapping the
// transport at the axios adapter level, exactly like client.spec.ts does for
// the shared client. The backend handler (rest.go handleClusterStatus) writes
// a flat JSON object with no { data, error, meta } envelope.
import { AxiosError } from 'axios'
import { describe, expect, it, vi } from 'vitest'

// The fixture is the SERVER's spelling (see web/src/api/__contract__/wire.json,
// produced by internal/grpc/wire_snapshot_test.go from the live handler): the
// gateway marshals state.ClusterNode / state.AssignmentSummary with
// encoding/json, so the keys are their json tags. It used to say `lastHeartbeat`
// / `nodeLoad` / `totalActive`, which is why this file passed while the cluster
// page rendered blanks on a real daemon.
const flatOk = {
	nodes: [{ id: 'n1', address: '10.0.0.1:9090', role: 'master', status: 'active', last_heartbeat: '2026-10-14T09:00:00Z', joined_at: '2026-10-13T09:00:00Z' }],
	summary: { counts: { pending: 1 }, node_load: { n1: 1 }, total_active: 1 },
	backend: 'postgres',
}

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

describe('systemApi.clusterStatus', { timeout: COLD_IMPORT_TIMEOUT }, () => {
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
		expect(result.summary.total_active).toBe(1)
		// The two keys the cluster page actually renders. They are asserted here
		// because a passthrough client cannot lose a key by design — this catches
		// a fixture that drifted from the wire, not a client that mangles one.
		expect(result.summary.node_load).toEqual({ n1: 1 })
		expect(result.nodes?.[0]?.last_heartbeat).toBe('2026-10-14T09:00:00Z')
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
