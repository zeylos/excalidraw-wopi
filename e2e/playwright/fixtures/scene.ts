// Scene helpers shared by every Playwright spec that draws on the
// excalidraw canvas and waits for the result to propagate: the
// dockerized-Drive smoke suite and the local fake-host convergence suite
// both drive the same rectangle tool and poll the same window.__excaTest
// accessor, so this fixture is their one shared copy.

import type { Page } from '@playwright/test'
import '../global.d.ts'

// CANVAS matches the interactive canvas layer the editor renders once it
// mounts; every helper below locates or reads through it.
export const CANVAS = '.excalidraw__canvas.interactive'

export const POLL_TIMEOUT_MS = 20_000
const POLL_INTERVAL_MS = 200

// SAVE_POLL_TIMEOUT_MS covers the worst case the save test can hit, the
// room's save cadence. The room's first-ever save fires immediately
// (saveDueLocked's lastSaveAttemptAt.IsZero() fast path) -- but a spurious
// empty PUT /api/board sync (useSync.ts's SERVER_API_SYNC_DELAY leading
// edge re-arming during the socket-connect/dedicated-syncer-election
// renders, before the test ever draws anything) can win that race instead.
// When that happens, the real content lands a moment later, and the
// room's 30s idle-flush window (room.idleFlushInterval) carries it to
// Drive well inside the 60s ServerSaveInterval throttle that is the
// ceiling for a continuously edited room. This budget keeps generous
// margin over that idle window, plus one background loop tick and
// network latency.
export const SAVE_POLL_TIMEOUT_MS = 75_000

export const EMPTY_SCENE = JSON.stringify({ type: 'excalidraw', version: 2, elements: [], appState: {}, files: {} })

// pollUntil is the shared wait primitive every spec in this suite uses
// instead of a fixed sleep: it re-reads fn until predicate passes or
// timeoutMs elapses.
export async function pollUntil<T>(fn: () => Promise<T>, predicate: (value: T) => boolean, timeoutMs = POLL_TIMEOUT_MS): Promise<T> {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const value = await fn()
    if (predicate(value)) {
      return value
    }
    if (Date.now() > deadline) {
      throw new Error(`pollUntil: timed out after ${timeoutMs}ms, last value: ${JSON.stringify(value)}`)
    }
    await new Promise(resolve => setTimeout(resolve, POLL_INTERVAL_MS))
  }
}

// drawRectangle selects the rectangle tool (a no-op for a read-only
// session, whose toolbar view mode hides), then drags one out clear of
// the shape-properties panel that opens on the canvas's left edge once a
// drawing tool is active. offset shifts the drag so two pages drawing at
// the same time land distinct rectangles instead of the same
// coordinates.
export async function drawRectangle(page: Page, offset: { x: number, y: number } = { x: 0, y: 0 }): Promise<void> {
  const rectangleTool = page.locator('[data-testid="toolbar-rectangle"]')
  if (await rectangleTool.count() > 0) {
    // The tool button sits under its own icon in the DOM hit-test order,
    // so a plain click reports the icon as intercepting it; force skips
    // that actionability check, which is safe here since the icon is
    // purely decorative chrome over the real input.
    await rectangleTool.click({ force: true })
  }

  const canvas = page.locator(CANVAS)
  const box = await canvas.boundingBox()
  if (!box) {
    throw new Error('drawRectangle: canvas has no bounding box')
  }

  const start = { x: box.x + 500 + offset.x, y: box.y + 300 + offset.y }
  const end = { x: box.x + 700 + offset.x, y: box.y + 500 + offset.y }
  await page.mouse.move(start.x, start.y)
  await page.mouse.down()
  await page.mouse.move(end.x, end.y, { steps: 10 })
  await page.mouse.up()
}

export async function elementCount(page: Page): Promise<number> {
  return page.evaluate(() => window.__excaTest?.getElements().length ?? 0)
}

// elementIds reads every element's id, for id-set convergence checks: a
// bare count on the receiving page can pass without the relay forwarding
// anything at all, if it happens to match from that page's own initial
// GET /api/board load.
export async function elementIds(page: Page): Promise<Set<string>> {
  const ids = await page.evaluate(() => (window.__excaTest?.getElements() ?? []).map(el => (el as { id: string }).id))
  return new Set(ids)
}
