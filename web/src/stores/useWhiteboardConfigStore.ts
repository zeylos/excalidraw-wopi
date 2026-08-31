/**
 * SPDX-FileCopyrightText: 2024 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// This store has no publicSharingToken or collabBackendUrl: the socket
// connects to the page origin at AppConfig.socketPath, so no per-board
// backend URL is needed. It also has no isEmbedded: this SPA has exactly
// one launch context, the WOPI iframe. isVersionPreview/versionSource/
// fileVersion/libraryRef do not exist either, since there is no
// version-preview or library feature here to support them.
// It holds no isReadOnly/setReadOnly: useSessionStore.isReadOnly (set once
// from the JWT canWrite claim) is this project's single source of truth
// for read-only state. It holds no zenModeEnabled/gridModeEnabled either:
// no feature here reads or sets them.

import { create } from 'zustand'
import { createResolvablePromise } from '../utils/createResolvablePromise'
import type { ExcalidrawInitialDataState } from '@excalidraw/excalidraw/types'

type InitialDataPromise = ReturnType<typeof createResolvablePromise>

interface WhiteboardConfigState {
	// Core state
	fileId: string
	fileName: string
	initialDataPromise: InitialDataPromise
	pendingInitialDataPromises: InitialDataPromise[]
	// True once resolveInitialData has fired for the current board load. A
	// dedicated syncer's leading-edge throttled server sync must wait for
	// this: PUTting before it is true would send the still-unhydrated,
	// empty scene as if it were the confirmed board state.
	initialDataResolved: boolean

	// Core actions
	setConfig: (
		config: Partial<Pick<WhiteboardConfigState, 'fileId' | 'fileName'>>,
	) => void
	// confirmed defaults to true (a genuine load result). A caller passes
	// false only after a failed load (a network error, not a confirmed-empty
	// 404): it still resolves the promise with an empty scene so the editor
	// can mount, but initialDataResolved stays false, so a dedicated syncer
	// never PUTs that empty scene back as the confirmed board state.
	resolveInitialData: (data: ExcalidrawInitialDataState, confirmed?: boolean) => void
	resetInitialDataPromise: () => void
	resetStore: () => void // Reset the entire store state
}

// Create the store without persistence
export const useWhiteboardConfigStore = create<WhiteboardConfigState>()((set, get) => ({
	// Core state
	fileId: '',
	fileName: '',
	initialDataPromise: createResolvablePromise(),
	pendingInitialDataPromises: [],
	initialDataResolved: false,

	// Core actions
	setConfig: (config) => {
		set(config)
	},

	resolveInitialData: (data: ExcalidrawInitialDataState, confirmed = true) => {
		const { initialDataPromise, pendingInitialDataPromises } = get()
		pendingInitialDataPromises.forEach((promise) => {
			promise.resolve(data)
		})
		initialDataPromise.resolve(data)
		const updates: Partial<WhiteboardConfigState> = { initialDataResolved: confirmed }
		if (pendingInitialDataPromises.length > 0) {
			updates.pendingInitialDataPromises = []
		}
		set(updates)
	},

	resetInitialDataPromise: () =>
		set((state) => ({
			pendingInitialDataPromises: [
				...state.pendingInitialDataPromises,
				state.initialDataPromise,
			],
			initialDataPromise: createResolvablePromise(),
			initialDataResolved: false,
		})),

	// Reset the entire store to its initial state
	resetStore: () => {
		// Keep the current fileId and fileName
		const { fileId, fileName } = get()
		set({
			// Preserve these values
			fileId,
			fileName,
			// Reset these values
			initialDataPromise: createResolvablePromise(),
			pendingInitialDataPromises: [],
			initialDataResolved: false,
		})
	},
}))
