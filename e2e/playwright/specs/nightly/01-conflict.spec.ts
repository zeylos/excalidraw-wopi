import { test, expect, type Browser, type BrowserContext, type Page } from '@playwright/test'
import '../../global.d.ts'
import { DriveClient, type DriveItem } from '../../fixtures/drive'
import { openSession } from '../../fixtures/launch'
import { CANVAS, EMPTY_SCENE, SAVE_POLL_TIMEOUT_MS, pollUntil, drawRectangle, elementIds } from '../../fixtures/scene'

// The conflict-banner path against a real dockerized Drive: an out-of-band
// upload (bypassing this service entirely) drifts the host's version away
// from what the room has retained, and a second user joining the room
// triggers the version check that surfaces it
// (internal/room/manager.go's Observe, lines 461-463, which sets
// pendingVersionCheck for a newly observed user). This exercises both
// resolution branches ConflictBanner.tsx offers a writer: Overwrite and
// Reload.

const MARKER_ELEMENT_ID = 'nightly-conflict-marker'

function markerScene(): string {
  return JSON.stringify({
    type: 'excalidraw',
    version: 2,
    elements: [{ id: MARKER_ELEMENT_ID, type: 'rectangle', x: 800, y: 800, width: 100, height: 100 }],
    appState: {},
    files: {},
  })
}

interface ConflictScenario {
  driveA: DriveClient
  item: DriveItem
  contextA: BrowserContext
  pageA: Page
  contextB: BrowserContext
  pageB: Page
  drawnElementId: string
}

// setupConflict builds one fresh Drive item, has A draw and save into it
// (proving the room has a version on record), shares it with a fresh user
// B, then uploads markerScene() straight to Drive's object store, out of
// band. Opening B's session last is what triggers the room's version
// check: see this file's own top comment.
async function setupConflict(browser: Browser, label: string): Promise<ConflictScenario> {
  const stamp = Date.now()
  const driveA = new DriveClient()
  await driveA.login(`nightly-conflict-a-${label}-${stamp}@example.com`)
  const item = await driveA.createItem(`nightly-conflict-${label}-${stamp}.excalidraw`)
  await driveA.uploadScene(item, EMPTY_SCENE)
  const wopiA = await driveA.wopiLaunch(item.id)
  const { context: contextA, page: pageA } = await openSession(browser, wopiA)

  await drawRectangle(pageA)
  const ids = await pollUntil(() => elementIds(pageA), s => s.size > 0)
  const drawnElementId = [...ids][0]
  await pollUntil(() => driveA.downloadFileContent(item), content => content.includes(drawnElementId), SAVE_POLL_TIMEOUT_MS)

  const driveB = new DriveClient()
  await driveB.login(`nightly-conflict-b-${label}-${stamp}@example.com`)
  const userBId = (await driveB.me()).id
  await driveA.shareEditor(item.id, userBId)

  await driveA.overwriteScene(item, markerScene())

  const wopiB = await driveB.wopiLaunch(item.id)
  const { context: contextB, page: pageB } = await openSession(browser, wopiB)

  return { driveA, item, contextA, pageA, contextB, pageB, drawnElementId }
}

// waitForReload waits out a full page reload (window.location.reload(),
// ConflictBanner.tsx's Reload path and the reload-required broadcast both
// trigger it) and confirms the SPA re-mounted, the same readiness fixtures/
// launch.ts's openEditor waits for after its own initial navigation.
async function waitForReload(page: Page): Promise<void> {
  await page.waitForSelector(CANVAS)
  await page.waitForFunction(() => window.__excaTest !== undefined)
}

test.describe.serial('conflict banner', () => {
  test('overwrite discards the outside edit and resumes saving', async ({ browser }) => {
    const { driveA, item, contextA, pageA, contextB, drawnElementId } = await setupConflict(browser, 'overwrite')
    try {
      await expect(pageA.getByRole('alert')).toContainText('This board changed outside this session')

      await pageA.getByRole('button', { name: 'Overwrite' }).click()
      await expect(pageA.getByRole('alert')).toHaveCount(0)

      // Overwrite forces the room's retained scene back onto the host on
      // its next background pass (internal/room/manager.go's
      // ResolveConflict), discarding the marker upload: A's drawn
      // element resurfaces in Drive's saved bytes.
      await pollUntil(() => driveA.downloadFileContent(item), content => content.includes(drawnElementId), SAVE_POLL_TIMEOUT_MS)

      await drawRectangle(pageA, { x: 300, y: 0 })
      const idsAfter = await pollUntil(() => elementIds(pageA), s => s.size > 1)
      const secondElementId = [...idsAfter].find(id => id !== drawnElementId)
      expect(secondElementId).toBeDefined()
      await pollUntil(() => driveA.downloadFileContent(item), content => content.includes(secondElementId as string), SAVE_POLL_TIMEOUT_MS)
    } finally {
      await contextA.close()
      await contextB.close()
    }
  })

  test('reload picks up the outside edit and drops local changes', async ({ browser }) => {
    const { pageA, contextA, pageB, contextB } = await setupConflict(browser, 'reload')
    try {
      await expect(pageA.getByRole('alert')).toContainText('This board changed outside this session')

      // Drawn after the banner appears, so this element is never saved to
      // the host (the room's save loop is paused while in conflict): the
      // reload must discard it, not let it come back through the
      // IndexedDB reconcile on the next load
      // (ConflictBanner.tsx's handleReload clears the local
      // hasPendingLocalChanges marker before reloading for exactly this).
      const idsBeforeDiscardedDraw = await elementIds(pageA)

      // ConflictBanner.tsx sits fixed at top-center, 460px wide at most,
      // over the toolbar (position fixed, top 16, left 50%, zIndex 10000):
      // drawRectangle's toolbar click lands on the banner instead of the
      // rectangle tool while the banner is showing. Select the tool with
      // the keyboard shortcut instead, and draw clear of both the banner
      // and the first rectangle.
      const canvasBox = await pageA.locator(CANVAS).boundingBox()
      if (!canvasBox) {
        throw new Error('reload test: canvas has no bounding box')
      }
      await pageA.mouse.click(canvasBox.x + 300, canvasBox.y + 550)
      await pageA.keyboard.press('r')
      await pollUntil(
        () => pageA.evaluate(() => (window.__excaTest?.getAppState() as { activeTool?: { type: string } })?.activeTool?.type),
        toolType => toolType === 'rectangle',
      )
      // Start right of the first rectangle (500..700) and below the
      // banner; the left properties panel opens with the tool and
      // swallows a drag that starts under it.
      await pageA.mouse.move(canvasBox.x + 850, canvasBox.y + 350)
      await pageA.mouse.down()
      await pageA.mouse.move(canvasBox.x + 1000, canvasBox.y + 470, { steps: 10 })
      await pageA.mouse.up()

      const idsAfterDiscardedDraw = await pollUntil(() => elementIds(pageA), s => s.size > idsBeforeDiscardedDraw.size)
      const discardedElementId = [...idsAfterDiscardedDraw].find(id => !idsBeforeDiscardedDraw.has(id))
      expect(discardedElementId).toBeDefined()

      // Registered before the click, so the wait cannot miss a
      // reload that starts faster than this call is set up (see
      // fixtures/launch.ts's boardLoaded comment for the same
      // pattern).
      const aReloaded = pageA.waitForEvent('load')
      // B never clicks anything: the server's reload-required
      // broadcast (internal/app.go wires ResolveConflict's reload
      // branch to rel.BroadcastToRoom) reaches every socket in the
      // room, so B reloads on its own.
      const bReloaded = pageB.waitForEvent('load')

      await pageA.getByRole('button', { name: 'Reload' }).click()
      await aReloaded
      await waitForReload(pageA)
      await pollUntil(() => elementIds(pageA), ids => ids.has(MARKER_ELEMENT_ID))
      await expect(pageA.getByRole('alert')).toHaveCount(0)
      expect((await elementIds(pageA)).has(discardedElementId as string)).toBe(false)

      await bReloaded
      await waitForReload(pageB)
      await pollUntil(() => elementIds(pageB), ids => ids.has(MARKER_ELEMENT_ID))
    } finally {
      await contextA.close()
      await contextB.close()
    }
  })
})
