// Command palette — the searchable action list behind Ctrl/Cmd+K.
//
// This module holds the part that can be reasoned about without a browser: what
// the commands are, how a query narrows them, and which ad-hoc commands a query
// like a change id implies. The component (CommandPalette.vue) owns only
// rendering and keyboard plumbing.
//
// Why a palette at all: the console's two most common keyboard tasks are "get me
// to a page" and "open this run whose id I just read in a log line". Today both
// mean navigating a rail and pasting into a field. Cursor's palette is the part
// of that product's design worth copying — not its colours.

/** One palette entry. `run` is the whole action; commands carry no view state. */
export interface PaletteCommand {
  id: string
  title: string
  /** Right-aligned hint: where it goes, or what it does. */
  hint?: string
  group: PaletteGroup
  icon: string
  /** Extra search terms that are not in the title (route paths, latin names). */
  keywords?: string[]
  run: () => void | Promise<void>
}

export type PaletteGroup = '跳转' | '导航' | '外观' | '维护'

/** Order groups are rendered in, so the list reads the same every time. */
export const GROUP_ORDER: readonly PaletteGroup[] = ['跳转', '导航', '外观', '维护']

/**
 * Scores one command against a query, or returns null when it does not match.
 *
 * Ranking is deliberately naive and predictable rather than clever: a command
 * whose title starts with the query beats one that contains it, which beats a
 * subsequence match ("csv" matching "change saved view"). Operators learn the
 * order within a few uses; fuzziness that reshuffles results teaches nothing.
 */
export function scoreCommand(cmd: PaletteCommand, query: string): number | null {
  if (!query) return 0
  const q = query.toLowerCase()
  const title = cmd.title.toLowerCase()

  if (title === q) return 1000
  if (title.startsWith(q)) return 900 - title.length
  const titleAt = title.indexOf(q)
  if (titleAt >= 0) return 700 - titleAt

  for (const kw of cmd.keywords ?? []) {
    const k = kw.toLowerCase()
    if (k === q) return 650
    if (k.startsWith(q)) return 600
    if (k.includes(q)) return 500
  }

  // Subsequence: every query character appears in order somewhere in the title.
  let qi = 0
  for (let i = 0; i < title.length && qi < q.length; i++) {
    if (title[i] === q[qi]) qi++
  }
  if (qi === q.length) return 300 - title.length

  return null
}

/**
 * Narrows and orders the list. With an empty query every command is returned in
 * its declared order (grouped by GROUP_ORDER), because a palette that shows
 * nothing until you type hides exactly what it is for: discovering actions.
 */
export function filterCommands(commands: PaletteCommand[], query: string): PaletteCommand[] {
  const q = query.trim()
  if (!q) {
    const rank = (g: PaletteGroup): number => GROUP_ORDER.indexOf(g)
    return [...commands].sort((a, b) => rank(a.group) - rank(b.group))
  }

  return commands
    .map((cmd) => ({ cmd, score: scoreCommand(cmd, q) }))
    .filter((r): r is { cmd: PaletteCommand; score: number } => r.score !== null)
    .sort((a, b) => b.score - a.score)
    .map((r) => r.cmd)
}

/**
 * True when a query looks like a machine identifier rather than prose. Ids in
 * this system are lower-case hex-ish with dashes (`chg-9f2a41d7`, `run-a3f1`);
 * prose ("数据库 切换") does not look like that. Used to decide whether the
 * palette should offer "open this run" ahead of "search for this text".
 */
export function looksLikeID(query: string): boolean {
  const q = query.trim()
  if (q.length < 6 || /\s/.test(q)) return false
  if (/[\u4e00-\u9fff]/.test(q)) return false
  return /^[A-Za-z0-9][A-Za-z0-9._:-]*$/.test(q)
}

/**
 * Commands implied by what the operator typed, ahead of the static list.
 *
 * A change id and a run id are the same key in this system (see MonitorView),
 * so "open" means two different views of the same row; offering both is honest,
 * and offering them is what turns the palette from a decorated nav bar into the
 * fastest way to reach a run someone just quoted in chat.
 */
export function jumpCommandsFor(query: string, go: (path: string) => void): PaletteCommand[] {
  const q = query.trim()
  if (!q) return []

  if (looksLikeID(q)) {
    return [
      {
        id: `jump-monitor:${q}`,
        title: `监控 run ${q}`,
        hint: '实时日志与批次推进',
        group: '跳转',
        icon: 'Monitor',
        run: () => go(`/monitor/${encodeURIComponent(q)}`),
      },
      {
        id: `jump-change:${q}`,
        title: `打开变更 ${q}`,
        hint: '变更详情',
        group: '跳转',
        icon: 'Document',
        run: () => go(`/changes/${encodeURIComponent(q)}`),
      },
    ]
  }

  return [
    {
      id: `search:${q}`,
      title: `在变更看板中搜索「${q}」`,
      hint: '按名称过滤',
      group: '跳转',
      icon: 'Search',
      run: () => go(`/changes?q=${encodeURIComponent(q)}`),
    },
  ]
}

/** Static commands: navigation, theme, maintenance. */
export function buildStaticCommands(opts: {
  go: (path: string) => void
  setTheme: (choice: 'light' | 'dark' | 'system') => void
  routes: Array<{ path: string; label: string; icon: string; keywords?: string[] }>
}): PaletteCommand[] {
  const nav: PaletteCommand[] = opts.routes.map((r) => ({
    id: `nav:${r.path}`,
    title: r.label,
    hint: r.path,
    group: '导航',
    icon: r.icon,
    keywords: [r.path, ...(r.keywords ?? [])],
    run: () => opts.go(r.path),
  }))

  const theme: PaletteCommand[] = [
    { id: 'theme:light', title: '切换到浅色主题', group: '外观', icon: 'Sunny', keywords: ['light', 'light mode'], run: () => opts.setTheme('light') },
    { id: 'theme:dark', title: '切换到深色主题', group: '外观', icon: 'Moon', keywords: ['dark', 'dark mode'], run: () => opts.setTheme('dark') },
    { id: 'theme:system', title: '主题跟随系统', group: '外观', icon: 'Laptop', keywords: ['system', 'auto'], run: () => opts.setTheme('system') },
  ]

  const maintenance: PaletteCommand[] = [
    {
      id: 'doctor:run',
      title: '运行诊断检查',
      hint: '/system',
      group: '维护',
      icon: 'FirstAidKit',
      keywords: ['doctor', 'diag', '诊断'],
      run: () => opts.go('/system?doctor=1'),
    },
    {
      id: 'audit:verify',
      title: '校验审计哈希链',
      hint: '/audit',
      group: '维护',
      icon: 'CircleCheck',
      keywords: ['audit', 'verify', 'hash', '哈希链'],
      run: () => opts.go('/audit?verify=1'),
    },
  ]

  return [...nav, ...theme, ...maintenance]
}
