// Narrow-viewport geometry contract for the core operator pages. These checks
// measure the rendered layout; screenshots alone cannot catch fixed-column
// overlays, vertical titles or an undisclosed horizontal scroll region.
export async function runNarrowGeometryChecks({ browser, newPage, open, check, sleep, SETTLE_DEADLINE_MS }) {
  const readTable = (page) => page.evaluate(() => {
    const table = document.querySelector('.el-table')
    const headers = [...document.querySelectorAll('.el-table__header-wrapper th')]
    const last = headers.filter((el) => !el.classList.contains('el-table-fixed-column--right')).at(-1)
    const fixed = document.querySelector('.el-table-fixed-column--right')
    return {
      rows: document.querySelectorAll('.el-table__row').length,
      labels: [...document.querySelectorAll('.el-table__header-wrapper th')].map((el) => el.textContent.trim()),
      tableRight: table ? Math.round(table.getBoundingClientRect().right) : 0,
      lastRight: last ? Math.round(last.getBoundingClientRect().right) : 0,
      fixedVisible: fixed ? getComputedStyle(fixed).display !== 'none' && fixed.getBoundingClientRect().width > 0 : false,
    }
  })

  for (const path of ['/approval', '/changes']) {
    for (const width of [1024, 768]) {
      const page = await newPage(browser, { width, height: 900 })
      await open(page, path)
      let layout = await readTable(page)
      const deadline = Date.now() + SETTLE_DEADLINE_MS
      while (layout.rows === 0 && Date.now() < deadline) {
        await sleep(200)
        layout = await readTable(page)
      }
      check(`${width} ${path} has no fixed-right overlay`, layout.rows > 0 && !layout.fixedVisible, JSON.stringify(layout))
      check(`${width} ${path} keeps the last column inside the table`, layout.lastRight > 0 && layout.lastRight <= layout.tableRight + 1,
        `lastRight=${layout.lastRight} tableRight=${layout.tableRight}`)
      await page.close()
    }
  }

  {
    const page = await newPage(browser, { width: 768, height: 900 })
    await open(page, '/changes/run-1000/ticket')
    const geometry = await page.evaluate(() => {
      const title = document.querySelector('.lv-page-head__title')?.getBoundingClientRect()
      const actions = document.querySelector('.lv-page-head__actions')?.getBoundingClientRect()
      const head = document.querySelector('.lv-page-head')
      return {
        titleHeight: title ? Math.round(title.height) : 0,
        titleText: document.querySelector('.lv-page-head__title')?.textContent?.trim() || '',
        actionsRight: actions ? Math.round(actions.right) : 0,
        direction: head ? getComputedStyle(head).flexDirection : '',
        viewport: window.innerWidth,
      }
    })
    check('768 ticket title stays horizontal', geometry.titleHeight > 0 && geometry.titleHeight <= 64 && geometry.titleText === '数据库变更发布单',
      JSON.stringify(geometry))
    check('768 ticket actions wrap inside the viewport', geometry.direction === 'column' && geometry.actionsRight > 0 && geometry.actionsRight <= geometry.viewport,
      JSON.stringify(geometry))
    await page.close()
  }

  {
    const page = await newPage(browser, { width: 768, height: 900 })
    await open(page, '/audit')
    const geometry = await page.evaluate(() => {
      const hint = document.querySelector('.lv-panel__hint')?.getBoundingClientRect()
      const panel = document.querySelector('.lv-panel')?.getBoundingClientRect()
      const body = document.querySelector('.el-table__body-wrapper')
      const scrollHint = document.querySelector('.table-scroll-hint')
      return {
        countHeight: hint ? Math.round(hint.height) : 0,
        countRight: hint ? Math.round(hint.right) : 0,
        panelRight: panel ? Math.round(panel.right) : 0,
        overflow: body ? body.scrollWidth > body.clientWidth : false,
        scrollHint: scrollHint?.textContent?.trim() || '',
        scrollHintVisible: scrollHint ? getComputedStyle(scrollHint).display !== 'none' : false,
      }
    })
    check('768 audit count stays on one line inside the panel', geometry.countHeight > 0 && geometry.countHeight <= 24 && geometry.countRight <= geometry.panelRight + 1,
      JSON.stringify(geometry))
    check('768 audit explains horizontal scrolling', !geometry.overflow || (geometry.scrollHintVisible && geometry.scrollHint.includes('左右')),
      JSON.stringify(geometry))
    await page.close()
  }

  {
    const page = await newPage(browser, { width: 768, height: 900 })
    await open(page, '/alerts')
    const layout = await readTable(page)
    check('768 alerts keep the last column inside the table', layout.rows > 0 && layout.lastRight > 0 && layout.lastRight <= layout.tableRight + 1,
      JSON.stringify(layout))
    await page.close()
  }
}
