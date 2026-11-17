import { readFileSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// sheetOverflow.spec.ts — 主内容区不许把横向滚动关掉。
//
// 背景（2026-11-17 实测，不是推断）：一份检索报告称「窄屏下表格页没有 @media
// 兜底，靠横向滚动」。逐条量过之后确认成立，而且范围比报告说的大——只有 4 个
// 文件带 `@media`。但「靠横向滚动」这件事本身**不是缺陷，而是一个当时没被
// 钉住的承重属性**：
//
//   `.sheet`（App.vue 的主内容滚动容器）只有 `overflow-y: auto`，没有写
//   `overflow-x`。按 CSS 规则，`visible` 遇上非 `visible` 的另一轴会被计算成
//   `auto`，所以横向溢出表现为**内容区里的横向滚动条**——表格右侧列够得到。
//
// 这条守卫防的是一个单项 CSS  regression：谁把 `.sheet` 加上
// `overflow-x: hidden`（一个"看起来在修布局警告"的改动），所有宽表格的右侧列
// 在同一时刻全部不可达——而在 1280px 宽屏上一切照常，没有任何测试会红。
//
// 这不是"窄屏美观"守卫（那需要真浏览器，见 docs 的已登记缺口）；它只钉住
// 一件事：**内容不许被裁掉**。

function locateApp(): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of ['src/App.vue', join('web', 'src', 'App.vue')]) {
      const p = join(dir, candidate)
      try {
        return readFileSync(p, 'utf8')
      } catch {
        // keep looking
      }
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate web/src/App.vue from ${process.cwd()}`)
}

const source = locateApp()

/** 取出 `.sheet { ... }` 规则体（花括号配平）。 */
function sheetBlock(): string {
  const at = source.indexOf('.sheet {')
  expect(at, 'App.vue must still declare .sheet').toBeGreaterThan(-1)
  const open = source.indexOf('{', at)
  let depth = 0
  for (let i = open; i < source.length; i++) {
    if (source[i] === '{') depth++
    else if (source[i] === '}') {
      depth--
      if (depth === 0) return source.slice(open + 1, i)
    }
  }
  throw new Error('.sheet rule is not closed in App.vue')
}

describe('sheet: wide content stays reachable', () => {
  it('does not clip the horizontal axis', () => {
    const block = sheetBlock()
    // Either spelling would cut off the right-hand columns of every wide table.
    expect(block, '.sheet must not declare overflow-x: hidden').not.toMatch(/overflow-x:\s*hidden/)
    expect(block, '.sheet must not declare overflow: hidden').not.toMatch(/overflow:\s*hidden/)
    // The vertical scrollbar is what the layout is built on; keep it.
    expect(block).toMatch(/overflow-y:\s*auto/)
  })

  it('lets the content region shrink so the scroll has somewhere to happen', () => {
    // min-width: 0 on the flex child: without it a flex item refuses to shrink
    // below its content width and the shell overflows instead of scrolling.
    expect(sheetBlock(), '.sheet keeps min-width: 0 (flex child must be allowed to shrink)')
      .toMatch(/min-width:\s*0/)
  })
})