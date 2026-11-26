// Unit tests for the diagnosis API client. Verifies the contract — diagnose/get
// issue POST to the gRPC-style DiagnosisService paths with protojson bodies —
// by swapping the axios adapter (same pattern as conversation.spec.ts).
import { describe, expect, it, vi } from 'vitest'

import { COLD_IMPORT_TIMEOUT } from '@/test/coldImport'

describe('diagnosisApi', { timeout: COLD_IMPORT_TIMEOUT }, () => {
  it('diagnose issues POST /DiagnosisService/Diagnose with target + opts', async () => {
    vi.resetModules()
    const mod = await import('./index')
    const client = await import('./client')

    client.axiosClient.defaults.adapter = async (config) => {
      expect(config.url).toBe('/DiagnosisService/Diagnose')
      expect(config.method).toBe('post')
      expect(config.data).toContain('"target":"10.0.1.11"')
      expect(config.data).toContain('"alertId":"alert-42"')
      return {
        data: {
          id: 'r-1', target: '10.0.1.11', status: 'healthy', trigger: 'alert',
          findings: [], recommendations: [], errors: [],
        },
        status: 200, statusText: 'OK', headers: {}, config,
      }
    }

    const result = await mod.diagnosisApi.diagnose('10.0.1.11', { alertId: 'alert-42' })
    expect(result.id).toBe('r-1')
    expect(result.findings).toEqual([])
  })

  it('get issues POST /DiagnosisService/GetDiagnosis with id', async () => {
    vi.resetModules()
    const mod = await import('./index')
    const client = await import('./client')
    client.axiosClient.defaults.adapter = async (config) => {
      expect(config.url).toBe('/DiagnosisService/GetDiagnosis')
      expect(config.data).toContain('"id":"r-1"')
      return {
        data: { id: 'r-1', target: 'h1', status: 'degraded', findings: [], recommendations: [], errors: [] },
        status: 200, statusText: 'OK', headers: {}, config,
      }
    }
    const result = await mod.diagnosisApi.get('r-1')
    expect(result.status).toBe('degraded')
  })
})
