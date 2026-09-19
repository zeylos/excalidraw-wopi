import { test, expect, type Page } from '@playwright/test'
import '../../global.d.ts'
import { CANVAS, POLL_TIMEOUT_MS, drawRectangle } from '../../fixtures/scene'

// The delay the slow-PUT route holds each save for. It must stay well
// above BLOCK_BUDGET_MS, so a main-thread block is unambiguous.
const SLOW_PUT_DELAY_MS = 2_000
// The budget for the visibilitychange dispatch itself. A synchronous XHR
// in that listener would hold the main thread for SLOW_PUT_DELAY_MS.
const BLOCK_BUDGET_MS = 1_000
// The quiet window that proves a back-forward cache pagehide sends
// nothing. It must stay under SERVER_API_SYNC_DELAY (10 s in
// web/src/hooks/useSync.ts), so the throttle's trailing call cannot land
// inside it and count as a teardown PUT.
const QUIET_WINDOW_MS = 1_500
// The window a listener-driven save must land in. It must stay under
// SERVER_API_SYNC_DELAY too: with a longer window the throttle's own
// trailing call satisfies the assertion, and a test that deletes the
// listener still passes.
const LISTENER_SAVE_TIMEOUT_MS = 6_000

async function openBoard(page: Page) {
  await page.goto('/fakewopi/launch?user=alice')
  await page.waitForSelector(CANVAS)
  await page.waitForFunction(() => window.__excaTest !== undefined)
}

function isBoardPut(url: string, method: string): boolean {
  return url.includes('/api/board') && method === 'PUT'
}

// recordBoardPuts counts every PUT /api/board the page issues. A teardown
// PUT is a synchronous XHR, so waitForResponse cannot observe it once the
// document goes away; the request event can.
function recordBoardPuts(page: Page): () => number {
  let count = 0
  page.on('request', request => {
    if (isBoardPut(request.url(), request.method())) {
      count++
    }
  })
  return () => count
}

// settleFirstSave draws one rectangle and waits for the save it triggers,
// so a later assertion starts from a quiet state. The launch already
// consumed the throttle's leading edge, so this usually waits the full
// SERVER_API_SYNC_DELAY for the trailing call.
async function settleFirstSave(page: Page): Promise<void> {
  await drawRectangle(page)
  await page.waitForResponse(
    res => isBoardPut(res.url(), res.request().method()),
    { timeout: POLL_TIMEOUT_MS },
  )
}

test.describe('local fake-host tab visibility', () => {
  test('a tab hide does not block the main thread', async ({ page }) => {
    await openBoard(page)

    // One writer alone is always the elected syncer, so this page owns
    // the save path the listener triggers. Drain the save it schedules
    // before the route below starts delaying one.
    await settleFirstSave(page)

    // A predicate, not a glob: the save URL carries a ?room= parameter,
    // which '**/api/board' does not match.
    await page.route(url => url.pathname.endsWith('/api/board'), async route => {
      if (route.request().method() !== 'PUT') {
        await route.continue()
        return
      }
      await new Promise(resolve => setTimeout(resolve, SLOW_PUT_DELAY_MS))
      await route.continue()
    })

    const putBoard = page.waitForResponse(
      res => isBoardPut(res.url(), res.request().method()),
      { timeout: LISTENER_SAVE_TIMEOUT_MS },
    )

    const startedAt = Date.now()
    await page.evaluate(() => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' })
      document.dispatchEvent(new Event('visibilitychange'))
    })
    const blockedMs = Date.now() - startedAt

    expect(blockedMs).toBeLessThan(BLOCK_BUDGET_MS)

    // The save still happens; it just runs in the sync worker.
    const response = await putBoard
    expect(response.status()).toBe(204)
  })

  test('a destroyed page flushes the scene on pagehide', async ({ page }) => {
    await openBoard(page)
    const boardPutCount = recordBoardPuts(page)
    await settleFirstSave(page)

    const before = boardPutCount()
    await page.evaluate(() => {
      window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: false }))
    })

    await expect.poll(boardPutCount).toBeGreaterThan(before)
  })

  test('a page entering the back-forward cache sends no teardown PUT', async ({ page }) => {
    await openBoard(page)
    const boardPutCount = recordBoardPuts(page)
    await settleFirstSave(page)

    const before = boardPutCount()
    await page.evaluate(() => {
      window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true }))
    })

    await page.waitForTimeout(QUIET_WINDOW_MS)
    expect(boardPutCount()).toBe(before)
  })
})
