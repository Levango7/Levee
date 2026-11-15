import { describe, expect, it, vi } from 'vitest'

// download.spec.ts — 导出发布单时的两个易错点：文件名与 Blob URL 回收。
//
// 两者都不会报错：文件名带路径分隔符时，浏览器会把路径截断（文件落到
// 别处，用户以为没生成）；Blob URL 不回收时，页面上看不出任何异常，只是
// 内存一直在涨。所以它们必须被测试钉住，而不是靠「看起来能用」。

import { downloadTextFile, safeFileName } from './download'

describe('safeFileName', () => {
  it('keeps a normal id unchanged', () => {
    expect(safeFileName('run-20261115-001')).toBe('run-20261115-001')
  })

  it('replaces path separators and reserved characters', () => {
    expect(safeFileName('a/b\\c:d*e?f"g<h>i|j')).not.toMatch(/[\\/:*?"<>|]/)
  })

  it('drops leading dots so the name cannot become hidden or relative', () => {
    expect(safeFileName('...secret')).toBe('secret')
  })

  it('falls back when nothing usable is left', () => {
    expect(safeFileName('   ')).toBe('document')
    expect(safeFileName('')).toBe('document')
  })
})

describe('downloadTextFile', () => {
  it('clicks an anchor carrying the sanitized file name and revokes the URL', async () => {
    const created: string[] = []
    const revoked: string[] = []
    const createObjectURL = vi.fn((_blob: Blob) => {
      const url = `blob:mock/${created.length}`
      created.push(url)
      return url
    })
    const revokeObjectURL = vi.fn((url: string) => {
      revoked.push(url)
    })
    vi.stubGlobal('URL', { ...URL, createObjectURL, revokeObjectURL })

    const clicks: HTMLAnchorElement[] = []
    const originalCreate = document.createElement.bind(document)
    vi.spyOn(document, 'createElement').mockImplementation((tag: string) => {
      const el = originalCreate(tag)
      if (tag === 'a') {
        clicks.push(el as HTMLAnchorElement)
        el.click = () => {
          el.dataset['clicked'] = '1'
        }
      }
      return el
    })

    downloadTextFile('run-1/ticket.md', '# 数据库变更发布单\n')

    expect(clicks).toHaveLength(1)
    expect(clicks[0]!.download).toBe('run-1-ticket.md')
    expect(clicks[0]!.dataset['clicked']).toBe('1')
    expect(created).toHaveLength(1)

    // The URL must be revoked — but not before the browser has taken the
    // blob, so the revocation is deferred to the next tick.
    expect(revoked).toHaveLength(0)
    await new Promise((resolve) => setTimeout(resolve, 0))
    expect(revoked).toEqual(created)

    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })
})
