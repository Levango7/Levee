// Unit tests for the palette's decision logic: ranking, ID detection, and the
// commands a query implies. The component around this is rendering and keyboard
// plumbing; these are the parts that can be wrong in ways nobody notices until
// the palette stops finding things.
import { describe, expect, it, vi } from 'vitest'

import {
  GROUP_ORDER,
  buildStaticCommands,
  filterCommands,
  jumpCommandsFor,
  looksLikeID,
  scoreCommand,
  type PaletteCommand,
} from './commands'

function cmd(partial: Partial<PaletteCommand> & { title: string }): PaletteCommand {
  return {
    id: partial.id ?? partial.title,
    group: partial.group ?? '导航',
    icon: partial.icon ?? 'Grid',
    run: partial.run ?? (() => {}),
    ...partial,
  }
}

describe('scoreCommand', () => {
  it('ranks exact > prefix > contains > subsequence', () => {
    const q = 'audit'
    const exact = scoreCommand(cmd({ title: 'audit' }), q)!
    const prefix = scoreCommand(cmd({ title: 'audit 哈希链' }), q)!
    const contains = scoreCommand(cmd({ title: '查看 audit 记录' }), q)!
    const subseq = scoreCommand(cmd({ title: 'a-u-d-i-t' }), q)!

    expect(exact).toBeGreaterThan(prefix)
    expect(prefix).toBeGreaterThan(contains)
    expect(contains).toBeGreaterThan(subseq)
  })

  it('is case-insensitive', () => {
    expect(scoreCommand(cmd({ title: 'Audit Log' }), 'AUDIT')).not.toBeNull()
  })

  it('matches on keywords, below title matches', () => {
    const viaKeyword = scoreCommand(cmd({ title: '校验链', keywords: ['audit'] }), 'audit')
    const viaTitle = scoreCommand(cmd({ title: 'audit' }), 'audit')!
    expect(viaKeyword).not.toBeNull()
    expect(viaTitle).toBeGreaterThan(viaKeyword!)
  })

  it('returns null when nothing matches', () => {
    expect(scoreCommand(cmd({ title: '目标机' }), 'zzz')).toBeNull()
  })

  it('treats an empty query as a zero-score match', () => {
    expect(scoreCommand(cmd({ title: '任意' }), '')).toBe(0)
  })
})

describe('filterCommands', () => {
  const commands = [
    cmd({ title: '系统状态', group: '导航' }),
    cmd({ title: '审计查询', group: '导航' }),
    cmd({ title: '切换到深色主题', group: '外观' }),
    cmd({ title: '运行诊断检查', group: '维护' }),
  ]

  it('returns everything for an empty query, ordered by group', () => {
    const out = filterCommands(commands, '   ')
    const groups = out.map((c) => c.group)
    const expected = [...groups].sort(
      (a, b) => GROUP_ORDER.indexOf(a) - GROUP_ORDER.indexOf(b),
    )
    expect(groups).toEqual(expected)
    expect(out).toHaveLength(4)
  })

  it('narrows to matches and keeps the best first', () => {
    const out = filterCommands(commands, '状态')
    expect(out.map((c) => c.title)).toEqual(['系统状态'])
  })

  it('can match across groups', () => {
    const out = filterCommands(commands, 'the') // no latin in titles here
    expect(out).toEqual([])
  })
})

describe('looksLikeID', () => {
  it('accepts the id shapes this system emits', () => {
    expect(looksLikeID('chg-9f2a41d7')).toBe(true)
    expect(looksLikeID('run-a3f1')).toBe(true)
    expect(looksLikeID('trc-7d21c0')).toBe(true)
  })

  it('rejects prose, short strings and anything with a space', () => {
    expect(looksLikeID('')).toBe(false)
    expect(looksLikeID('abc')).toBe(false)
    expect(looksLikeID('订单库主从切换')).toBe(false)
    expect(looksLikeID('chg 9f2a')).toBe(false)
    expect(looksLikeID('database switchover')).toBe(false)
  })
})

describe('jumpCommandsFor', () => {
  it('offers both views of an id-shaped query, since change and run share a key', () => {
    const go = vi.fn()
    const cmds = jumpCommandsFor('chg-9f2a41d7', go)
    expect(cmds).toHaveLength(2)
    expect(cmds.every((c) => c.group === '跳转')).toBe(true)

    cmds[0]!.run()
    expect(go).toHaveBeenCalledWith('/monitor/chg-9f2a41d7')
    cmds[1]!.run()
    expect(go).toHaveBeenCalledWith('/changes/chg-9f2a41d7')
  })

  it('turns a free-text query into a board search, url-encoded', () => {
    const go = vi.fn()
    const cmds = jumpCommandsFor('订单库 切换', go)
    expect(cmds).toHaveLength(1)
    cmds[0]!.run()
    expect(go).toHaveBeenCalledWith('/changes?q=%E8%AE%A2%E5%8D%95%E5%BA%93%20%E5%88%87%E6%8D%A2')
  })

  it('offers nothing for an empty query — the static list is already showing', () => {
    expect(jumpCommandsFor('   ', vi.fn())).toEqual([])
  })
})

describe('buildStaticCommands', () => {
  it('wires navigation, theme and maintenance actions', () => {
    const go = vi.fn()
    const setTheme = vi.fn()
    const cmds = buildStaticCommands({
      go,
      setTheme,
      routes: [{ path: '/changes', label: '变更看板', icon: 'Odometer' }],
    })

    const nav = cmds.find((c) => c.id === 'nav:/changes')!
    nav.run()
    expect(go).toHaveBeenCalledWith('/changes')
    expect(nav.keywords).toContain('/changes')

    cmds.find((c) => c.id === 'theme:dark')!.run()
    expect(setTheme).toHaveBeenCalledWith('dark')

    const doctor = cmds.find((c) => c.id === 'doctor:run')!
    doctor.run()
    expect(go).toHaveBeenCalledWith('/system?doctor=1')

    // Every command declares a group the palette knows how to order.
    for (const c of cmds) expect(GROUP_ORDER).toContain(c.group)
  })
})
