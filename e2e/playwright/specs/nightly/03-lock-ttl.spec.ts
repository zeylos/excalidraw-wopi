import { execSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test, expect } from '@playwright/test'
import '../../global.d.ts'
import { DriveClient } from '../../fixtures/drive'
import { openSession } from '../../fixtures/launch'
import { EMPTY_SCENE, SAVE_POLL_TIMEOUT_MS, pollUntil, drawRectangle, elementIds } from '../../fixtures/scene'

// This spec waits about 35 real minutes by design: it is the only way to
// prove the WOPI lock survives past its own 1800s TTL through the room's
// refresh loop (internal/hostadapter/drive.go's LockRefreshInterval, 10
// min). There is no time-compression knob, so nightly is the only suite
// that runs it.

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..', '..', '..')
const composeFile = path.join(repoRoot, 'e2e', 'compose.yaml')

const WOPI_LOCK_TIMEOUT_SECONDS = 1800
const TOTAL_WAIT_MINUTES = 32
// Measured refreshes land at loop minutes 10/20/30 (internal/hostadapter/
// drive.go's LockRefreshInterval, 10 min). A save inside the first ten
// minutes can push the first refresh later, since every successful save
// also resets the lock's own refresh timestamp, so minute 14 gives that
// margin instead of asserting right at the boundary.
const MID_CHECK_MINUTE = 14
const CHECK_INTERVAL_MS = 60_000

// redisCli runs redis-cli inside the e2e stack's redis container (see
// e2e/compose.yaml; every service binds network_mode: host, but compose
// exec still addresses a container by its service name).
function redisCli(...args: string[]): string {
  const command = ['docker', 'compose', '-f', composeFile, 'exec', '-T', 'redis', 'redis-cli', ...args].join(' ')
  return execSync(command, { encoding: 'utf8' }).trim()
}

// lockKey mirrors django_redis's prefix:version:key scheme over Drive's
// own wopi_lock:<itemId> key (wopi/services/lock.py's LockService):
// KEY_PREFIX defaults to "drive", VERSION defaults to 1.
function lockKey(itemId: string): string {
  return `drive:1:wopi_lock:${itemId}`
}

function lockTTL(itemId: string): number {
  return Number(redisCli('TTL', lockKey(itemId)))
}

function lockExists(itemId: string): boolean {
  return redisCli('EXISTS', lockKey(itemId)) === '1'
}

test('the WOPI lock survives past its own TTL through the room refresh loop', async ({ browser }) => {
  test.setTimeout(45 * 60_000)

  const driveA = new DriveClient()
  await driveA.login(`nightly-lock-ttl-${Date.now()}@example.com`)
  const item = await driveA.createItem(`nightly-lock-ttl-${Date.now()}.excalidraw`)
  await driveA.uploadScene(item, EMPTY_SCENE)
  const wopi = await driveA.wopiLaunch(item.id)
  const { context, page } = await openSession(browser, wopi)

  try {
    await drawRectangle(page)
    const firstIds = await pollUntil(() => elementIds(page), s => s.size > 0, SAVE_POLL_TIMEOUT_MS)
    const drawnElementId = [...firstIds][0]
    await pollUntil(() => driveA.downloadFileContent(item), content => content.includes(drawnElementId), SAVE_POLL_TIMEOUT_MS)

    expect(lockExists(item.id)).toBe(true)
    const initialTTL = lockTTL(item.id)
    expect(initialTTL).toBeGreaterThan(0)
    expect(initialTTL).toBeLessThanOrEqual(WOPI_LOCK_TIMEOUT_SECONDS)

    const ttlSeries: number[] = []
    let sawIncrease = false

    for (let minute = 1; minute <= TOTAL_WAIT_MINUTES; minute++) {
      await new Promise(resolve => setTimeout(resolve, CHECK_INTERVAL_MS))

      const ttl = lockTTL(item.id)
      // redis-cli TTL returns -2 for a missing key; assert here so a
      // dropped lock fails fast at the minute it happened, not silently
      // at minute 32's final checks.
      expect(ttl, `lock key ${lockKey(item.id)} is missing at minute ${minute} (TTL returned -2)`).not.toBe(-2)
      ttlSeries.push(ttl)
      // A liveness trail in the trace: proves the loop ran every
      // minute, not just that the assertions below happened to pass.
      console.log(`lock-ttl minute ${minute}: TTL=${ttl}s`)

      expect(await page.getByRole('alert').count()).toBe(0)
      expect(await page.evaluate(() => window.__excaTest?.getCollabState().status)).toBe('online')

      if (ttlSeries.length > 1 && ttl > ttlSeries[ttlSeries.length - 2]) {
        sawIncrease = true
      }

      if (minute === MID_CHECK_MINUTE) {
        // A refresh resets the TTL back to 1800s; without one it
        // can only ever count down from its starting value.
        expect(sawIncrease).toBe(true)
      }
    }

    expect(lockExists(item.id)).toBe(true)
    expect(lockTTL(item.id)).toBeGreaterThan(0)

    // The refreshed lock still admits a save past the 30-minute mark
    // of its original TTL.
    await drawRectangle(page, { x: 300, y: 0 })
    const secondIds = await pollUntil(() => elementIds(page), s => s.size > firstIds.size)
    const secondElementId = [...secondIds].find(id => !firstIds.has(id))
    expect(secondElementId).toBeDefined()
    await pollUntil(() => driveA.downloadFileContent(item), content => content.includes(secondElementId as string), SAVE_POLL_TIMEOUT_MS)
  } finally {
    await context.close()
  }
})
