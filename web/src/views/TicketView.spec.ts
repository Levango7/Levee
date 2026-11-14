import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// TicketView.spec.ts — 发布单页面读的字段名必须与 wire 一致。
//
// 这条守卫有具体来历：第一版 TicketView 按「想象出来的形状」写的 DTO，把
// workflow/template 写成 workflowFile/templateName、给 SQL 步骤多写了两个
// proto 里根本没有的字段（batchNo / hostCount），又按 approval 记录自己那套
// 字段名写了 minApprovers / initiator / expiresAt。TypeScript 不报错
// （自己声明的类型当然自洽），页面也不报错——它只是**每一格都是空白**。
//
// 顶层契约（wireContract.spec.ts）只能看见七个分段名，看不见分段里的字段，
// 所以这里把「分段内部的关键字段名」钉住：视图读什么、DTO 声明什么，
// 两边都必须与 proto（= wire 的 protojson 拼写）对上。

function locatePath(rel: string): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [rel, join('web', rel)]) {
      const p = join(dir, candidate)
      try {
        readFileSync(p)
        return p
      } catch {
        // keep looking
      }
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate ${rel} from ${process.cwd()}`)
}

const view = readFileSync(locatePath('src/views/TicketView.vue'), 'utf8')
const types = readFileSync(locatePath('src/types/levee.ts'), 'utf8')

/** 每个分段对应的 DTO 接口名（wire 的分段名 → TS 接口名）。 */
const SECTION_DTO: Record<string, string> = {
  change: 'ChangeSection',
  sql: 'SQLSection',
  rollback: 'RollbackSection',
  window: 'WindowSection',
  approval: 'ApprovalSection',
  execution: 'ExecutionSection',
  audit: 'AuditSection',
}

/** 每个分段里「页面上真的会出现」的字段——少一个字段，页面就少一格。 */
const SECTION_FIELDS: Record<string, string[]> = {
  change: ['id', 'label', 'status', 'priority', 'environment', 'creator', 'workflow', 'template', 'planHash', 'planState', 'createdAt', 'updatedAt'],
  sql: ['steps', 'review', 'hasSql'],
  rollback: ['strategy', 'onFailure', 'verifyAfter', 'snapshotPaths', 'stepUndos', 'hasUndo'],
  window: ['enforcedAt', 'persistedPerRun', 'note'],
  approval: ['level', 'status', 'approvers', 'decisions', 'pendingCount', 'approvedCount', 'rejectedCount', 'matchesPlan', 'legacyApproval', 'staleApproval', 'hasApproval'],
  execution: ['verdict', 'reason', 'batches', 'steps', 'totalTargets', 'hasEvidence'],
  audit: ['valid', 'count', 'legacyCount', 'verified', 'unverifiable', 'verifyError'],
}

/** 这些是**旧 DTO 自己编的**字段名，proto 里没有；它们回来就意味着又漂了。 */
const INVENTED_FIELDS = [
  'workflowFile',
  'templateName',
  'minApprovers',
  'excludeInitiator',
  'hostCount',
  'reviewedAt',
  'reviewer',
  'ruleId',
  'ruleName',
  'sqlSnippet',
]

/** 取出某个 interface 的声明块（只看它自己的那一段，不吞邻居）。 */
function interfaceBlock(name: string): string {
  const decl = new RegExp(`export interface ${name}\\s*\\{([\\s\\S]*?)\\n\\}`)
  return decl.exec(types)?.[1] ?? ''
}

describe('ticket view: the fields it reads are the fields the wire sends', () => {
  it('the DTO declares every field of every section', () => {
    for (const [section, fields] of Object.entries(SECTION_FIELDS)) {
      const interfaceName = SECTION_DTO[section]!
      const block = interfaceBlock(interfaceName)
      expect(block, `${interfaceName} must be declared`).not.toBe('')
      for (const field of fields) {
        expect(
          new RegExp(`^ {2}${field}\\??:`, 'm').test(block),
          `${interfaceName}.${field} must be declared (wire sends it)`,
        ).toBe(true)
      }
    }
  })

  it('the invented field names stay gone', () => {
    // 只看**发布单分段**的接口：Change 这样的老 DTO 本来就有 templateName /
    // workflowFile（那是另一条 wire 路径的字段），不是这里要禁的东西。
    const sectionBlocks = Object.values(SECTION_DTO)
      .map((name) => interfaceBlock(name))
      .join('\n')
    for (const field of INVENTED_FIELDS) {
      expect(
        sectionBlocks,
        `${field} is not a ticket wire field — it must not come back`,
      ).not.toContain(`  ${field}:`)
      expect(view, `${field} is not a ticket wire field — the view must not read it`).not.toContain(
        `.${field}`,
      )
    }
  })

  it('the view reads the section fields it declares', () => {
    for (const [section, fields] of Object.entries(SECTION_FIELDS)) {
      for (const field of fields) {
        expect(
          view,
          `TicketView must read ${section}.${field} (the wire sends it)`,
        ).toContain(`.${field}`)
      }
    }
  })

  it('the honest three states are rendered, not flattened', () => {
    // 计划不可读 / legacy 审批 / 校验出错：这三格一旦被压平成「没有」，
    // 页面就会对读者说谎。三个标记都必须出现在视图里。
    expect(view).toContain('planState')
    expect(view).toContain('unreadable')
    expect(view).toContain('legacyApproval')
    expect(view).toContain('staleApproval')
    expect(view).toContain('verifyError')
    expect(view).toContain('unverifiable')
  })
})
