/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

import { create } from 'zustand'
import type { ExcalidrawImperativeAPI, BinaryFiles } from '@excalidraw/excalidraw/types'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState } from '@excalidraw/excalidraw/types'
import { loadConfig } from '../config'
import { useCollaborationStore, type CollaborationConnectionStatus } from './useCollaborationStore'

interface ExcalidrawStore {
	excalidrawAPI: ExcalidrawImperativeAPI | null

	setExcalidrawAPI: (api: ExcalidrawImperativeAPI) => void
	resetExcalidrawAPI: () => void
}

// window.__excaTest is a read-only scene accessor for the local Playwright
// suite. A read-only getter discloses nothing a script on this page could
// not already read straight off the Excalidraw API, but it stays gated on a
// runtime signal rather than always-on, so a production build never exposes
// it to an arbitrary embedding page by default.
declare global {
	interface Window {
		__excaTest?: {
			getElements: () => readonly ExcalidrawElement[]
			getAppState: () => AppState
			getFiles: () => BinaryFiles
			getCollabState: () => { status: CollaborationConnectionStatus, isInRoom: boolean }
		}
	}
}

// shouldExposeTestHooks gates window.__excaTest on the Go server's own
// testHooks config flag (internal/launch's WithTestHooks, set for
// --fake-host dev mode and for EXCALIDRAW_WOPI_TEST_HOOKS=1's dockerized-
// Drive smoke suite), plus a plain Vite dev-mode check for `npm run dev`.
function shouldExposeTestHooks(): boolean {
	return import.meta.env.DEV || loadConfig()?.testHooks === true
}

export const useExcalidrawStore = create<ExcalidrawStore>((set) => ({
	excalidrawAPI: null,

	setExcalidrawAPI: (api: ExcalidrawImperativeAPI) => {
		set({ excalidrawAPI: api })
		if (shouldExposeTestHooks()) {
			window.__excaTest = {
				getElements: () => api.getSceneElements(),
				getAppState: () => api.getAppState(),
				getFiles: () => api.getFiles(),
				getCollabState: () => {
					const { status, isInRoom } = useCollaborationStore.getState()
					return { status, isInRoom }
				},
			}
		}
	},
	resetExcalidrawAPI: () => {
		set({ excalidrawAPI: null })
		delete window.__excaTest
	},
}))
