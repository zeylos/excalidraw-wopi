import { test, expect, type Page, type Browser, type BrowserContext } from '@playwright/test'
import '../../global.d.ts'
import { DriveClient, type DriveItem } from '../../fixtures/drive'
import { openSession } from '../../fixtures/launch'
import { CANVAS, EMPTY_SCENE, SAVE_POLL_TIMEOUT_MS, pollUntil, drawRectangle, elementCount, elementIds } from '../../fixtures/scene'

// The PR gate: launch from a real dockerized Drive, two
// browsers draw and converge, paste an image, save and check that the
// item version moves, reopen, and check that a read-only user cannot
// draw. Runs against the stack `make e2e-up` brings up (compose.yaml,
// Drive on :8000, our own binary on :8080) -- see e2e/README.md.
//
// The suite needs EXCALIDRAW_WOPI_TEST_HOOKS=1 on our binary (set in
// e2e/env/excalidraw.env) for window.__excaTest: the real Drive launch
// path (unlike the local suite's --fake-host mode) does not set it by
// default (internal/app/testhooks.go).

// A well-known, minimal 1x1 transparent PNG: small enough to keep the
// image scenario fast, and needs no fixture file on disk.
const ONE_PX_PNG_BASE64
  = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII='

// pasteImage dispatches a synthetic paste ClipboardEvent carrying a PNG
// File, the same technique excalidraw's own test suite uses to drive its
// native paste-to-insert-image handling: Excalidraw listens for `paste`
// on `document`, so this needs no dependency on a hidden file input
// whose DOM shape is an internal library detail.
//
// App.pasteFromClipboard bails out unless document.activeElement sits
// inside the excalidraw container (its outer div carries tabIndex=0) and
// the point at App.lastViewportPosition resolves to the canvas element
// (App only updates lastViewportPosition from real pointer-move events).
// A real click on the canvas satisfies both before the synthetic event
// fires.
async function pasteImage(page: Page): Promise<void> {
  const canvas = page.locator(CANVAS)
  const box = await canvas.boundingBox()
  if (!box) {
    throw new Error('pasteImage: canvas has no bounding box')
  }
  await page.mouse.click(box.x + 500, box.y + 300)

  await page.evaluate(async (base64) => {
    const byteChars = atob(base64)
    const bytes = new Uint8Array(byteChars.length)
    for (let i = 0; i < byteChars.length; i++) {
      bytes[i] = byteChars.charCodeAt(i)
    }
    const file = new File([bytes], 'smoke.png', { type: 'image/png' })
    const dataTransfer = new DataTransfer()
    dataTransfer.items.add(file)
    const event = new ClipboardEvent('paste', { clipboardData: dataTransfer, bubbles: true, cancelable: true })
    // Dispatch on the canvas, not document: Excalidraw's paste handler
    // may be bound on an inner container rather than document itself, and
    // a bubbling dispatch from a descendant reaches every ancestor
    // listener either way.
    const target = document.querySelector('.excalidraw') ?? document.body
    target.dispatchEvent(event)
  }, ONE_PX_PNG_BASE64)
}

async function imageElementIds(page: Page): Promise<Set<string>> {
  const ids = await page.evaluate(() =>
    (window.__excaTest?.getElements() ?? [])
      .filter(el => (el as { type?: string }).type === 'image')
      .map(el => (el as { id: string }).id))
  return new Set(ids)
}

async function fileCount(page: Page): Promise<number> {
  return page.evaluate(() => Object.keys(window.__excaTest?.getFiles() ?? {}).length)
}

function setDifference<T>(a: Set<T>, b: Set<T>): Set<T> {
  return new Set([...a].filter(item => !b.has(item)))
}

function containsAll<T>(set: Set<T>, required: Set<T>): boolean {
  return [...required].every(item => set.has(item))
}

// flushAndClose simulates a real browser-tab close before tearing the
// session down. useSync.ts's PUT /api/board sync is throttled to 10s
// (SERVER_API_SYNC_DELAY), so a draw right before close can otherwise
// still be unsent when the room saves: production covers that case with
// a 'beforeunload' listener (useSync.ts's handleBeforeUnload) that flushes
// the cached scene through a synchronous XHR. Playwright's
// BrowserContext.close() tears the page down directly, without firing
// 'beforeunload', so this dispatches it first to run the same flush path
// a real tab close would.
async function flushAndClose(session: { context: BrowserContext, page: Page }): Promise<void> {
  await session.page.evaluate(() => window.dispatchEvent(new Event('beforeunload')))
  await session.context.close()
}

test.describe.serial('dockerized-Drive smoke', () => {
  let driveA: DriveClient
  let driveB: DriveClient
  let mainItemId: string
  let saveItem: DriveItem
  let savedElementId: string
  let userBId: string

  test.beforeAll(async () => {
    // Two logins plus two wopiLaunch polls (each bounded by
    // fixtures/drive.ts's own WOPI_POLL_TIMEOUT_MS, 60s) can together
    // approach the suite's 120s default; give this setup step its own,
    // more generous budget.
    test.setTimeout(180_000)

    driveA = new DriveClient()
    await driveA.login(`smoke-alice-${Date.now()}@example.com`)

    // mainItem carries the convergence and image scenarios: their
    // assertions only need elements to propagate, not a fresh-room save
    // timing guarantee.
    const mainItem = await driveA.createItem(`smoke-main-${Date.now()}.excalidraw`)
    mainItemId = mainItem.id
    await driveA.uploadScene(mainItem, EMPTY_SCENE)
    // The first wopi/ poll proves Drive routed .excalidraw to us
    // (discovery ingested), the same gate e2e-seed exercises.
    await driveA.wopiLaunch(mainItemId)

    // saveItem stays undrawn until the save-version test below, so that
    // test's draw is this room's very first-ever save and lands on
    // room.Manager's no-throttle fast path (see that test's comment).
    // Its saved content then carries into the reopen and read-only
    // scenarios, matching this suite's flow (save, reopen, check
    // read-only, in that order, on the same file).
    saveItem = await driveA.createItem(`smoke-save-${Date.now()}.excalidraw`)
    await driveA.uploadScene(saveItem, EMPTY_SCENE)
    await driveA.wopiLaunch(saveItem.id)

    driveB = new DriveClient()
    await driveB.login(`smoke-bob-${Date.now()}@example.com`)
    userBId = (await driveB.me()).id
  })

  async function openWriterSession(browser: Browser, itemId: string) {
    const wopi = await driveA.wopiLaunch(itemId)
    return openSession(browser, wopi)
  }

  test('two writers converge on a drawn rectangle', async ({ browser }) => {
    const a = await openWriterSession(browser, mainItemId)
    const b = await openWriterSession(browser, mainItemId)

    try {
      const beforeIdsA = await elementIds(a.page)
      const beforeIdsB = await elementIds(b.page)

      // This scenario calls for both browsers drawing at the same time,
      // not one page drawing while the other only observes.
      await Promise.all([
        drawRectangle(a.page, { x: 0, y: 0 }),
        drawRectangle(b.page, { x: 250, y: 0 }),
      ])

      const afterIdsA = await pollUntil(() => elementIds(a.page), ids => ids.size > beforeIdsA.size)
      const afterIdsB = await pollUntil(() => elementIds(b.page), ids => ids.size > beforeIdsB.size)
      const newFromA = setDifference(afterIdsA, beforeIdsA)
      const newFromB = setDifference(afterIdsB, beforeIdsB)

      // Convergence: each page must end up holding both writers' new
      // elements, by id -- a count that happens to match can pass without
      // the relay forwarding anything (e.g. from B's own initial board
      // load), so this compares the actual element-id sets.
      await pollUntil(() => elementIds(a.page), ids => containsAll(ids, newFromB))
      await pollUntil(() => elementIds(b.page), ids => containsAll(ids, newFromA))

      expect(newFromA.size).toBeGreaterThan(0)
      expect(newFromB.size).toBeGreaterThan(0)
    } finally {
      await a.context.close()
      await b.context.close()
    }
  })

  test('an inserted image propagates with its file data', async ({ browser }) => {
    const a = await openWriterSession(browser, mainItemId)
    const b = await openWriterSession(browser, mainItemId)

    try {
      const beforeIds = await imageElementIds(a.page)

      await pasteImage(a.page)

      const afterIdsA = await pollUntil(() => imageElementIds(a.page), ids => ids.size > beforeIds.size)
      const newImageIds = setDifference(afterIdsA, beforeIds)
      const newImageId = [...newImageIds][0]
      // Id containment, not a count match: see the convergence test's own
      // comment for why a count alone is not proof the relay forwarded it.
      await pollUntil(() => imageElementIds(b.page), ids => ids.has(newImageId))
      // The image element converging is not enough on its own (excalidraw
      // elements and their binary file bytes travel separately: IMAGE_ADD
      // over the websocket, no blob store); assert B's own getFiles()
      // actually holds the pasted PNG's bytes too.
      await pollUntil(() => fileCount(b.page), n => n > 0)

      expect(newImageIds.size).toBeGreaterThan(0)
    } finally {
      await a.context.close()
      await b.context.close()
    }
  })

  test("a save moves Drive's item version", async ({ browser }) => {
    // playwright.smoke.config.ts's own test timeout (120s) already covers
    // SAVE_POLL_TIMEOUT_MS's worst case (see that constant's comment) with
    // margin to spare, so this test needs no timeout override of its own.

    const before = await driveA.itemDetail(saveItem.id)

    const { context, page } = await openWriterSession(browser, saveItem.id)
    await drawRectangle(page)
    const ids = await pollUntil(() => elementIds(page), s => s.size > 0)
    savedElementId = [...ids][0]
    // Closing the only open session empties the room
    // (internal/room/manager.go's OnLeave -> roomEmpty), which schedules
    // its close-grace flush. This file's room has never saved before this
    // test (saveItem is untouched since beforeAll's upload), so
    // saveDueLocked's lastSaveAttemptAt.IsZero() branch already made the
    // very first save due the moment any scene landed, well before this
    // close. flushAndClose is what gets the drawn rectangle itself to the
    // room before that flush: see its own comment.
    //
    // "Any scene" above is the catch: useSync.ts mounts with an onChange
    // fire or two of its own, from useSync's SERVER_API_SYNC_DELAY-
    // throttled PUT /api/board sync re-arming its leading edge across the
    // socket-connect/dedicated-syncer-election renders before this test
    // ever draws anything, each one a legitimate (if empty) sync of the
    // room's genuinely-still-empty scene. The room's once-only fast path
    // does not know to wait for a "real" edit, so it can spend itself on
    // one of those first -- the poll below must not stop at the first
    // updatedAt change, only at the one that actually carries the
    // rectangle (size growing past before.size proves that; Drive's save
    // format is just `{elements,files}`, still smaller than before's
    // full EMPTY_SCENE envelope until real content lands).
    await flushAndClose({ context, page })

    // The poll below already proves updatedAt moved (size only grows past
    // before.size on a genuine PutFile, per the comment above), so no
    // separate updatedAt assertion is needed here.
    const after = await pollUntil(
      () => driveA.itemDetail(saveItem.id),
      detail => (detail.size ?? 0) > (before.size ?? 0),
      SAVE_POLL_TIMEOUT_MS,
    )
    expect(after.size ?? 0).toBeGreaterThan(before.size ?? 0)

    // The stronger check: read the actual saved bytes back from Drive's
    // object store (see downloadFileContent's own comment for why this
    // suite cannot use Drive's own download route in this stack) and
    // confirm they carry this test's own drawn element, not just that some
    // PutFile landed.
    const savedContent = await driveA.downloadFileContent(saveItem)
    expect(savedContent).toContain(savedElementId)
  })

  test('reopening the file loads the drawn scene', async ({ browser }) => {
    // The strongest proof of persistence: the bytes Drive actually holds,
    // independent of the room.Manager's in-memory retained scene, which
    // this test would otherwise be reading straight through -- it reopens
    // well inside closeGrace (10s, internal/room/manager.go), so a UI
    // reopen alone proves nothing about what Drive actually saved.
    const savedContent = await driveA.downloadFileContent(saveItem)
    expect(savedContent).toContain(savedElementId)

    // Secondary check: the UI reopen still round-trips the same element.
    const { context, page } = await openWriterSession(browser, saveItem.id)
    try {
      const ids = await pollUntil(() => elementIds(page), s => s.size > 0)
      expect(ids.has(savedElementId)).toBe(true)
    } finally {
      await context.close()
    }
  })

  test('a read-only user cannot draw but still receives live updates', async ({ browser }) => {
    await driveA.shareReadOnly(saveItem.id, userBId)

    const writer = await openWriterSession(browser, saveItem.id)
    const readerWopi = await driveB.wopiLaunch(saveItem.id)
    const reader = await openSession(browser, readerWopi)

    try {
      const viewModeEnabled = await reader.page.evaluate(() => window.__excaTest?.getAppState().viewModeEnabled)
      expect(viewModeEnabled).toBe(true)

      const beforeOwnAttempt = await elementCount(reader.page)
      const writerBeforeReaderAttempt = await elementCount(writer.page)
      await drawRectangle(reader.page)
      // No positive event to wait for here: the whole point is that view
      // mode drops the drag, so this is a bounded negative-assertion
      // grace window, not a poll. Checking the writer's own count too
      // exercises the relay's server-side drop of a read-only session's
      // broadcast: if the relay ever forwarded it, the writer's scene
      // would pick it up.
      await reader.page.waitForTimeout(500)
      expect(await elementCount(reader.page)).toBe(beforeOwnAttempt)
      expect(await elementCount(writer.page)).toBe(writerBeforeReaderAttempt)

      const beforeWriterIds = await elementIds(writer.page)
      await drawRectangle(writer.page)
      const afterWriterIds = await pollUntil(() => elementIds(writer.page), ids => ids.size > beforeWriterIds.size)
      const newWriterId = [...setDifference(afterWriterIds, beforeWriterIds)][0]
      await pollUntil(() => elementIds(reader.page), ids => ids.has(newWriterId))
    } finally {
      await writer.context.close()
      await reader.context.close()
    }
  })
})
