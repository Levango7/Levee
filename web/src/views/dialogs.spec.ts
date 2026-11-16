import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'

import { describe, expect, it } from 'vitest'

// dialogs.spec.ts — 两类「只在窄屏/快手上」现形的前端缺陷，变成机器可查。
//
// 这两类都是一次性 grep 发现的，不是任何测试抓到的：
//
//   ① `el-dialog width="640px"` 这类写死像素宽：宽屏上完全正常，窄屏下横向
//      溢出（对话框比视口还宽）。修法是把宽度写成 `min(Npx, 100vw - 48px)`。
//   ② 提交按钮没有 `:loading`：单击正常，快手双击会重复提交。CSV 批量导入
//      更糟——它是一条**逐行请求的循环**，第二次点击会再跑一遍整个循环，
//      每台主机被加两次。
//
// 二者的共同点是：**都不影响"能跑"**，所以功能测试与构建都绿，只有真机/真人
// 才会遇到。这正是结构性守卫存在的理由。本文件读入库源码（同 regionStates /
// priorityDisplay 一类守卫的做法），把两个形状钉住。

function locateViewsDir(): string {
  let dir = process.cwd()
  for (let hop = 0; hop < 4; hop++) {
    for (const candidate of [join('src', 'views'), join('web', 'src', 'views')]) {
      const p = join(dir, candidate)
      try {
        readdirSync(p)
        return p
      } catch {
        // keep looking
      }
    }
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  throw new Error(`cannot locate web/src/views from ${process.cwd()}`)
}

const viewsDir = locateViewsDir()

/** 源码里整行注释与模板注释先剥掉：注释里提到 `<el-dialog` 不算用法。 */
function codeOnly(source: string): string {
  return source
    .split('\n')
    .filter((line) => !line.trim().startsWith('//') && !line.trim().startsWith('<!--') && !line.trim().startsWith('*'))
    .join('\n')
}

function viewNames(): string[] {
  return readdirSync(viewsDir).filter((f) => f.endsWith('.vue')).sort()
}

function source(name: string): string {
  return codeOnly(readFileSync(join(viewsDir, name), 'utf8'))
}

describe('dialogs: the two shapes that only bite on a narrow screen or a fast hand', () => {
  it('no dialog pins its width to a fixed pixel value', () => {
    // A fixed width is fine on a desktop and overflows on a phone. The
    // responsive form is `min(<px>, 100vw - <margin>)`.
    for (const name of viewNames()) {
      const src = source(name)
      // The capture group is always present when the match succeeds, but
      // `noUncheckedIndexedAccess` types it as `string | undefined` — and
      // RegExp.test() takes a plain string. The filter is the narrowing, not
      // a behaviour change: a match without the group cannot happen.
      const widths = [...src.matchAll(/<el-dialog[^>]*?\bwidth="([^"]+)"/g)]
        .map((m) => m[1])
        .filter((w): w is string => w !== undefined)
      for (const w of widths) {
        expect(
          /^min\(/.test(w),
          `${name} pins a dialog width to "${w}" — use min(${w}, 100vw - 48px) so a narrow viewport keeps it inside`,
        ).toBe(true)
      }
    }
  })

  it('every dialog submit button shows its own loading state', () => {
    // A primary button that fires a `submit*` handler is a write. Without
    // :loading a second click starts a second request — and a submission loop
    // (the CSV import) runs it twice over the same rows.
    for (const name of viewNames()) {
      const src = source(name)
      // Match an el-button whose handler submits, and check the same tag for
      // a loading binding. Scoped per-tag (not per-file) so a file with two
      // submit buttons cannot borrow one guard from the other.
      for (const tag of [...src.matchAll(/<el-button\b[^>]*>/g)].map((m) => m[0])) {
        const submits = /@click="(submit\w+|import\w*|add\w*)"/.test(tag)
        if (!submits) continue
        expect(
          /:loading=/.test(tag),
          `${name} has a submit button without a loading state: ${tag.trim()}`,
        ).toBe(true)
      }
    }
  })
})