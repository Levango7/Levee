import { describe, expect, it } from 'vitest'

import { STEP_STATES, STEP_TONE, buildTargetMap, cellSummary } from './targetMap'

const planBatches = [
  { index: 1, hosts: ['web-01', 'web-02', 'web-03'] },
  { index: 2, hosts: ['web-04'] },
]

const steps = (rows: Array<[string, number, string, string, number?]>) =>
  rows.map(([host, batchNo, status, stepName, durationMs]) => ({
    host,
    batchNo,
    status,
    stepName,
    durationMs: durationMs ?? 0,
  }))

const progress = (nos: number[]): Array<{ batch_no: number; status: string; total_hosts: number; succeeded: number; failed: number }> =>
  nos.map((batch_no) => ({ batch_no, status: 'running', total_hosts: 1, succeeded: 0, failed: 0 }))

describe('buildTargetMap', () => {
  it('joins the plan assignment with the step rows', () => {
    const map = buildTargetMap(planBatches, steps([['web-01', 1, 'success', 'upgrade', 1200]]), progress([1]))
    expect(map.batchNos).toEqual([1, 2])
    // Every host the plan assigns gets a row, including the ones that have not
    // started: an empty cell is information, a missing row is a lie.
    expect(map.rows.map((r) => r.host).sort()).toEqual(['web-01', 'web-02', 'web-03', 'web-04'])
    expect(map.untouched.sort()).toEqual(['web-02', 'web-03', 'web-04'])
    const web01 = map.rows.find((r) => r.host === 'web-01')
    expect(web01?.cells[1]?.state).toBe('success')
    expect(web01?.cells[1]?.stepName).toBe('upgrade')
    // A host that has not started is NOT reported as a success.
    expect(map.rows.find((r) => r.host === 'web-02')?.worst).toBe('pending')
  })

  it('puts the worst state in the row summary and sorts worst-first', () => {
    const map = buildTargetMap(
      planBatches,
      steps([
        ['web-01', 1, 'success', 'upgrade'],
        ['web-02', 1, 'failed', 'upgrade', 900],
        ['web-03', 1, 'running', 'upgrade'],
      ]),
      progress([1]),
    )
    // worst-first: failed, running, then the not-started host, then the success
    expect(map.rows.map((r) => r.host)).toEqual(['web-02', 'web-03', 'web-04', 'web-01'])
    const worstOf = (host: string) => map.rows.find((r) => r.host === host)?.worst
    expect(worstOf('web-02')).toBe('failed')
    expect(worstOf('web-03')).toBe('running')
    expect(worstOf('web-01')).toBe('success')
    // The host that never started is pending, not "success because nothing broke"
    expect(worstOf('web-04')).toBe('pending')
  })

  it('keeps a column for a batch that has only progress rows', () => {
    // A pre-plan row (or a plan that was lost) must not vanish: hiding the
    // column would hide work that actually happened.
    const map = buildTargetMap([], steps([['web-09', 3, 'failed', 'upgrade']]), progress([3]))
    expect(map.batchNos).toEqual([3])
    expect(map.rows).toHaveLength(1)
    expect(map.rows[0]?.host).toBe('web-09')
  })

  it('reads an unknown status as pending rather than inventing a state', () => {
    const map = buildTargetMap(planBatches, steps([['web-01', 1, 'weird', 'upgrade']]), progress([1]))
    expect(map.rows.find((r) => r.host === 'web-01')?.cells[1]?.state).toBe('pending')
  })

  it('does not fold one host into a cell for a batch it was not in', () => {
    const map = buildTargetMap(planBatches, steps([['web-01', 2, 'success', 'upgrade']]), progress([1, 2]))
    const web01 = map.rows.find((r) => r.host === 'web-01')
    expect(web01?.cells[1]).toBeUndefined()
    expect(web01?.cells[2]?.state).toBe('success')
  })
})

describe('STEP_TONE', () => {
  it('covers the whole vocabulary (a keyed Record, not a switch)', () => {
    for (const s of STEP_STATES) {
      expect(STEP_TONE[s]).toBeTruthy()
    }
  })
})

describe('cellSummary', () => {
  it('names the state, the step, the duration and the exit code when present', () => {
    expect(cellSummary({ state: 'failed', stepName: 'upgrade', durationMs: 900, exitCode: 1 })).toBe(
      '失败 · upgrade · 900 ms · exit=1',
    )
  })

  it('stays short for a plain success', () => {
    expect(cellSummary({ state: 'success', stepName: 'upgrade', durationMs: 0 })).toBe('成功 · upgrade')
  })
})
