import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// 断网策略 Q3：分区期（控制面数据库不可达）状态面必须带标记，且**不得**
// 把「读不到」说成「0 个活跃执行」。
//
// 这层守卫盯的是诚实性，不是样式：分区期的健康面板如果继续显示 0，值班
// 的人会得到一个关于平台的肯定陈述，而事实是「看不到」。后端因此带
// controlPlaneReachable / runCountsAvailable / partitionReason 三个字段，
// 前端必须真的用它们：不可达时显示告警，计数显示「未知」。

function locatePath(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      if (readFileSyncSafe(p) !== null) return p
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

function readFileSyncSafe(path: string): string | null {
  try {
    return readFileSync(path, 'utf8')
  } catch {
    return null
  }
}

const viewPath = locatePath('src/views/SystemView.vue')
const typesPath = locatePath('src/types/levee.ts')
const view = readFileSync(viewPath, 'utf8')
const types = readFileSync(typesPath, 'utf8')

describe('system status: partition marker', () => {
  it('the wire type carries the partition fields', () => {
    expect(types).toContain('controlPlaneReachable')
    expect(types).toContain('runCountsAvailable')
    expect(types).toContain('partitionReason')
  })

  it('the view reads the control-plane marker', () => {
    expect(view).toContain('controlPlaneReachable')
    expect(view).toContain('partitionReason')
  })

  it('run counts are hidden behind an availability check, not printed as zero', () => {
    expect(view).toContain('countsAvailable')
    expect(view).toMatch(/countsAvailable \? status\.activeRuns : '未知'/)
    expect(view).toMatch(/countsAvailable \? status\.pausedRuns : '未知'/)
  })

  it('a partitioned platform shows an explicit warning, not a silent zero', () => {
    expect(view).toContain('控制面数据库不可达')
  })
})
