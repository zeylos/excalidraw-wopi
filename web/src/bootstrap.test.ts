import { describe, expect, it } from 'vitest'
import { resolveBootstrap } from './bootstrap'

// This is a smoke test for the bootstrap decision, not a render test of
// <App>. Mounting the stock <Excalidraw> under vitest/happy-dom fails at
// import time (the dev build's `import open-color.json` needs a `type:
// "json"` attribute this project's Node/Vite setup does not supply), so
// there is no App-render path to assert "the Excalidraw canvas mounts" or
// "viewModeEnabled follows canWrite" against. resolveBootstrap is the pure
// function main.tsx runs before it touches the DOM or any store, and it is
// what decides both facts this test can still check for: whether the app
// renders at all, and what canWrite value would reach useSessionStore (which
// is what viewModeEnabled is derived from downstream, per useReadOnlyState).

const baseConfig = {
  fileId: 'file-1',
  fileName: 'board.excalidraw',
  userId: 'user-1',
  userName: 'Alice',
  canWrite: true,
  sessionToken: 'token-abc',
  apiBase: '/api',
  socketPath: '/socket.io',
  maxImageBytes: 10 * 1024 * 1024,
}

describe('resolveBootstrap', () => {
  it('is not ready when config is null', () => {
    expect(resolveBootstrap(null)).toEqual({ ready: false })
  })

  it('derives the session, whiteboard config, and App props from a writable config', () => {
    const decision = resolveBootstrap(baseConfig)

    expect(decision).toEqual({
      ready: true,
      session: {
        sessionToken: 'token-abc',
        userId: 'user-1',
        userName: 'Alice',
        canWrite: true,
      },
      whiteboardConfig: {
        fileId: 'file-1',
        fileName: 'board.excalidraw',
      },
      appProps: {
        apiBase: '/api',
        socketPath: '/socket.io',
        maxImageBytes: 10 * 1024 * 1024,
      },
    })
  })

  it('carries canWrite=false through to the derived session (drives viewModeEnabled downstream)', () => {
    const decision = resolveBootstrap({ ...baseConfig, canWrite: false })

    expect(decision.ready).toBe(true)
    expect(decision.ready && decision.session.canWrite).toBe(false)
  })
})
