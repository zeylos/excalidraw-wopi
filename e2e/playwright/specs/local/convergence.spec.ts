import { test, expect, type Page, type BrowserContext } from '@playwright/test'
import '../../global.d.ts'
import { CANVAS, POLL_TIMEOUT_MS, pollUntil, drawRectangle, elementCount } from '../../fixtures/scene'

// openBoard drives the fake-host launch flow exactly as a browser does:
// GET the auto-submitting form, let it POST itself to /launch, and wait
// for the editor to mount, including bob's read-only path.
async function openBoard(page: Page, user: 'alice' | 'bob') {
  await page.goto(`/fakewopi/launch?user=${user}`)
  await page.waitForSelector(CANVAS)
  await page.waitForFunction(() => window.__excaTest !== undefined)
}

async function openContextPage(browser: import('@playwright/test').Browser, user: 'alice' | 'bob'): Promise<{ context: BrowserContext, page: Page }> {
  const context = await browser.newContext()
  const page = await context.newPage()
  await openBoard(page, user)
  return { context, page }
}

test.describe('local fake-host convergence', () => {
  test('two writers converge on a drawn rectangle', async ({ browser }) => {
    const a = await openContextPage(browser, 'alice')
    const b = await openContextPage(browser, 'alice')

    try {
      const beforeA = await elementCount(a.page)
      const beforeB = await elementCount(b.page)

      await drawRectangle(a.page)

      const afterA = await pollUntil(() => elementCount(a.page), n => n > beforeA)
      // Convergence: page B must observe the exact same element count page
      // A ended up with, not just "more than before" (a race could add an
      // unrelated stray element and still pass a looser check).
      await pollUntil(() => elementCount(b.page), n => n === afterA)

      expect(afterA).toBeGreaterThan(beforeA)
      expect(await elementCount(b.page)).toBeGreaterThan(beforeB)
    } finally {
      await a.context.close()
      await b.context.close()
    }
  })

  test('the syncer flushes a drawn rectangle to the Go server', async ({ browser }) => {
    const a = await openContextPage(browser, 'alice')
    const b = await openContextPage(browser, 'alice')

    try {
      // The relay elects one of the two connected writers as the syncer;
      // either page may end up posting the REST save, so race both
      // pages' network traffic for it.
      const putBoard = Promise.race([
        a.page.waitForResponse(res => res.url().includes('/api/board') && res.request().method() === 'PUT', { timeout: POLL_TIMEOUT_MS }),
        b.page.waitForResponse(res => res.url().includes('/api/board') && res.request().method() === 'PUT', { timeout: POLL_TIMEOUT_MS }),
      ])

      await drawRectangle(a.page)

      const response = await putBoard
      expect(response.status()).toBe(204)

      // internal/room's Manager owns the host save loop: it retains the
      // scene handlePutBoard stores and flushes it to the WOPI host
      // (X-WOPI-Override: PUT against /fakewopi/files/f-local) on its
      // own background schedule, asynchronously from the PUT /api/board
      // response above. The flush is asynchronous, so the test polls
      // /fakewopi/_state until putCount >= 1 instead of asserting on the
      // PUT response's timing.
      await pollUntil(
        () => a.page.request.get('/fakewopi/_state').then(res => res.json()),
        (state: { putCount: number }) => state.putCount >= 1,
      )
    } finally {
      await a.context.close()
      await b.context.close()
    }
  })

  test('a read-only user cannot draw but still receives live updates', async ({ browser }) => {
    const writer = await openContextPage(browser, 'alice')
    const reader = await openContextPage(browser, 'bob')

    try {
      const viewModeEnabled = await reader.page.evaluate(() => window.__excaTest?.getAppState().viewModeEnabled)
      expect(viewModeEnabled).toBe(true)

      const beforeOwnAttempt = await elementCount(reader.page)
      await drawRectangle(reader.page)
      // Give any (incorrect) element creation a moment to land before
      // asserting nothing changed; there is no positive event to wait for
      // here, since the whole point is that view mode drops the drag.
      await reader.page.waitForTimeout(500)
      expect(await elementCount(reader.page)).toBe(beforeOwnAttempt)

      // The reader still receives element propagation from the writer:
      // cursors and, by the same relay path, element broadcasts stay
      // visible to a read-only session. Cursor visibility itself is hard
      // to assert end-to-end here, so this checks element propagation
      // only.
      const beforeWriter = await elementCount(writer.page)
      await drawRectangle(writer.page)
      const afterWriter = await pollUntil(() => elementCount(writer.page), n => n > beforeWriter)
      await pollUntil(() => elementCount(reader.page), n => n === afterWriter)
    } finally {
      await writer.context.close()
      await reader.context.close()
    }
  })
})
