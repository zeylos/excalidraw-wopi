import { test, expect, type Page } from '@playwright/test'
import '../../global.d.ts'
import { io } from 'socket.io-client'
import { DriveClient } from '../../fixtures/drive'
import { openSession } from '../../fixtures/launch'
import { EMPTY_SCENE, pollUntil, drawRectangle, elementIds } from '../../fixtures/scene'

// The session JWT's own expiry, independent of Drive's WOPI access token:
// the minted session's exp claim comes straight from the posted
// access_token_ttl (internal/launch/launch.go); maxSessionTTL is only a
// 10-hour ceiling on that value, not its source. A short TTL here tests
// the Go server's own enforcement. Drive's access token stays valid for
// its normal, much longer lifetime throughout this spec; only the
// session-JWT layer is under test.

const SHORT_TTL_MS = 180_000
const EXPIRY_GRACE_MS = 5_000
const WAIT_CHUNK_MS = 10_000

interface EwConfig {
  fileId: string
  apiBase: string
  sessionToken: string
}

async function readConfig(page: Page): Promise<EwConfig> {
  return page.evaluate(() => JSON.parse(document.getElementById('ew-config')!.textContent!))
}

// fetchBoardStatus runs the fetch from inside the page, carrying the
// browser's own current bearer token: this is the "in-page fetch" this
// spec's assertions call for, as opposed to the node-side socket and
// re-launch checks further down.
async function fetchBoardStatus(page: Page, boardUrl: string, token: string): Promise<number> {
  return page.evaluate(
    async ([url, bearer]) => {
      const response = await fetch(url, { headers: { Authorization: `Bearer ${bearer}` } })
      return response.status
    },
    [boardUrl, token] as const,
  )
}

async function waitUntil(deadline: number, chunkMs = WAIT_CHUNK_MS): Promise<void> {
  while (Date.now() < deadline) {
    const remaining = deadline - Date.now()
    await new Promise(resolve => setTimeout(resolve, Math.min(chunkMs, remaining)))
  }
}

test('the session JWT expires at its own access_token_ttl', async ({ browser }) => {
  test.setTimeout(6 * 60_000)

  const driveA = new DriveClient()
  await driveA.login(`nightly-token-expiry-${Date.now()}@example.com`)
  const item = await driveA.createItem(`nightly-token-expiry-${Date.now()}.excalidraw`)
  await driveA.uploadScene(item, EMPTY_SCENE)
  const wopi = await driveA.wopiLaunch(item.id)
  const shortenedTtl = Date.now() + SHORT_TTL_MS

  let drawnElementId = ''
  const { context, page } = await openSession(browser, { ...wopi, accessTokenTtl: shortenedTtl })
  try {
    const config = await readConfig(page)
    const boardUrl = `${config.apiBase}/board?room=${encodeURIComponent(config.fileId)}`

    await drawRectangle(page)
    const ids = await pollUntil(() => elementIds(page), s => s.size > 0)
    drawnElementId = [...ids][0]

    expect(await fetchBoardStatus(page, boardUrl, config.sessionToken)).toBe(200)
    expect(await page.evaluate(() => window.__excaTest?.getCollabState().status)).toBe('online')

    await waitUntil(shortenedTtl + EXPIRY_GRACE_MS)

    expect(await fetchBoardStatus(page, boardUrl, config.sessionToken)).toBe(401)

    // A fresh node-side handshake with the now-stale JWT must be
    // refused at connect time, with exactly the frontend's circuit-
    // breaker string (internal/relay/relay.go's authErrorMessage). A
    // fresh handshake is used here, not context.setOffline on the open
    // page, because Chromium's offline emulation does not reliably
    // sever an already-open WebSocket.
    const socket = io('http://localhost:8080', { auth: { token: config.sessionToken }, transports: ['websocket'], reconnection: false })
    try {
      const connectError = await new Promise<Error>((resolve, reject) => {
        socket.once('connect_error', resolve)
        socket.once('connect', () => reject(new Error('socket connected with an expired session token')))
      })
      expect(connectError.message).toBe('Authentication error')
    } finally {
      socket.close()
    }

    // A node-side re-POST of the launch form, still carrying the same
    // stale access_token_ttl, must also fail: /launch's own
    // parseAccessTokenTTL rejects a non-future value with 400,
    // regardless of whether Drive's own access_token is still valid.
    const relaunchResponse = await fetch(wopi.launchUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ access_token: wopi.accessToken, access_token_ttl: String(shortenedTtl) }),
    })
    expect(relaunchResponse.status).toBe(400)

    // Deliberate pin of the current behavior: expiry is enforced at
    // handshake and REST call time, not mid-session. This page's
    // socket, connected before the token expired, stays online.
    expect(await page.evaluate(() => window.__excaTest?.getCollabState().status)).toBe('online')
  } finally {
    await context.close()
  }

  const freshWopi = await driveA.wopiLaunch(item.id)
  const fresh = await openSession(browser, freshWopi)
  try {
    const freshIds = await pollUntil(() => elementIds(fresh.page), s => s.size > 0)
    expect(freshIds.has(drawnElementId)).toBe(true)
  } finally {
    await fresh.context.close()
  }
})
