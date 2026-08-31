// SPDX-License-Identifier: AGPL-3.0-or-later

// This project has a single launch path: no runtime-mode dispatch.
// resolveBootstrap is the pure decision step main.tsx runs between
// loadConfig() and rendering; see main.tsx for why it lives in its own
// module.

import type { AppConfig } from './config'

export type BootstrapDecision =
  | { ready: false }
  | {
      ready: true
      session: Pick<AppConfig, 'sessionToken' | 'userId' | 'userName' | 'canWrite'>
      whiteboardConfig: Pick<AppConfig, 'fileId' | 'fileName'>
      appProps: Pick<AppConfig, 'apiBase' | 'socketPath' | 'maxImageBytes'>
    }

/**
 * Pure decision step between loadConfig() and rendering: given the parsed
 * config (or null), decides whether the app can launch and, if so, derives
 * the store payloads and App props from it. Kept in its own module, apart
 * from main.tsx's DOM and store side effects, so it stays testable without
 * mounting React or touching the document.
 */
export function resolveBootstrap(config: AppConfig | null): BootstrapDecision {
  if (!config) {
    return { ready: false }
  }

  return {
    ready: true,
    session: {
      sessionToken: config.sessionToken,
      userId: config.userId,
      userName: config.userName,
      canWrite: config.canWrite,
    },
    whiteboardConfig: {
      fileId: config.fileId,
      fileName: config.fileName,
    },
    appProps: {
      apiBase: config.apiBase,
      socketPath: config.socketPath,
      maxImageBytes: config.maxImageBytes,
    },
  }
}
