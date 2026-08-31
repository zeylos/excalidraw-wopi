import type { Browser, BrowserContext, Page } from '@playwright/test'
import type { WopiLaunch } from './drive'
import '../global.d.ts'

const CANVAS = '.excalidraw__canvas.interactive'
const BOARD_LOAD_TIMEOUT_MS = 20_000
const ROOM_JOIN_TIMEOUT_MS = 20_000

function escapeAttr(value: string): string {
  return value.replace(/&/g, '&amp;').replace(/"/g, '&quot;')
}

// openEditor drives the real Drive launch flow exactly as
// WopiEditorFrame.tsx's iframe form POST does (access_token,
// access_token_ttl hidden fields, POSTed to wopi.launchUrl): it sets a
// tiny self-submitting form as the page's content, in place of Drive's
// nested iframe, since the smoke suite only needs the resulting
// excalidraw-wopi SPA, not Drive's own preview chrome around it.
export async function openEditor(page: Page, wopi: WopiLaunch): Promise<void> {
  // The submit runs from a setTimeout, not inline during script parsing: a
  // synchronous form.submit() races setContent's own wait for the page's
  // load event, since the navigation it starts can tear the still-loading
  // document's execution context down before that wait settles --
  // intermittently throwing "execution context destroyed". Deferring the
  // submit to the next task lets setContent's load wait resolve first, so
  // the navigation becomes a clean, separate step instead of a race.
  await page.setContent(`<!DOCTYPE html>
<html><body>
<form id="f" method="POST" action="${escapeAttr(wopi.launchUrl)}">
<input type="hidden" name="access_token" value="${escapeAttr(wopi.accessToken)}">
<input type="hidden" name="access_token_ttl" value="${wopi.accessTokenTtl}">
</form>
<script>setTimeout(() => document.getElementById('f').submit(), 0)</script>
</body></html>`)

  // Registered before the deferred submit fires (see the comment above),
  // so this listener is live no matter how fast the app's own initial
  // board fetch runs once the navigation lands.
  const boardLoaded = page.waitForResponse(
    response => response.request().method() === 'GET' && new URL(response.url()).pathname.endsWith('/board'),
    { timeout: BOARD_LOAD_TIMEOUT_MS },
  )
  // waitForSelector below can outlast BOARD_LOAD_TIMEOUT_MS (its own default
  // timeout is 30s), so boardLoaded can reject before anything awaits it.
  // This no-op catch keeps that rejection from becoming an unhandled one and
  // crashing the worker; the real await further down still sees the
  // rejection and fails the test, since a catch here does not consume it.
  boardLoaded.catch(() => {})

  await page.waitForSelector(CANVAS)
  await page.waitForFunction(() => window.__excaTest !== undefined)
  await boardLoaded

  // A caller that draws the moment this function returns (every
  // openWriterSession call does) needs the collaboration socket to have
  // actually joined the room, not merely connected: useCollaboration only
  // marks isInRoom true once the relay confirms the join with
  // room-user-change. Without this wait, a draw right after openEditor can
  // race that join and never reach the relay.
  await page.waitForFunction(() => window.__excaTest?.getCollabState().isInRoom === true, undefined, { timeout: ROOM_JOIN_TIMEOUT_MS })
}

// openSession opens a fresh browser context and launches wopi in it,
// mirroring specs/local/convergence.spec.ts's openContextPage but against
// a real Drive-issued launch payload instead of the fake host's.
export async function openSession(browser: Browser, wopi: WopiLaunch): Promise<{ context: BrowserContext, page: Page }> {
  const context = await browser.newContext()
  const page = await context.newPage()
  await openEditor(page, wopi)
  return { context, page }
}
