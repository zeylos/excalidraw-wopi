/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// fetchDataFromServer is utils/fetchWhiteboardSnapshot.ts's
// fetchWhiteboardSnapshot(apiBase, token): the Go board API
// (internal/boardapi) answers with the raw scene, with no `libraryRef` or
// `scrollToContent` field (this project has no library or version-preview
// feature; scrollToContent below explains the replacement).
//
// isReadOnly reads from useSessionStore (bootstrap-immutable, set once
// from the JWT canWrite claim) rather than useWhiteboardConfigStore, so
// this hook's data policy never depends on another hook's mount order.
//
// scrollToContent: the server sends no scroll-to-content flag, so this
// hook computes the simplest faithful equivalent: scroll to content only
// when the resolved scene is non-empty (an empty board has nothing to
// scroll to, and default-centers instead).
//
// restoreElements is a static top-level import from `@excalidraw/excalidraw`
// (stock 0.18 export).
//
// The three near-identical "resolve after a 50ms settle delay" blocks
// (data found / no data / error) are one closure, resolveAfterDelay, to
// remove the duplication; the delay itself (letting the Excalidraw
// component finish mounting before the first resolveInitialData) is
// unchanged.

import { useCallback, useEffect, useRef, useState } from 'react'
import { useWhiteboardConfigStore } from '../stores/useWhiteboardConfigStore'
import { useExcalidrawStore } from '../stores/useExcalidrawStore'
import { useSessionStore } from '../stores/useSessionStore'
import { useSyncStore } from '../stores/useSyncStore'
import { db } from '../database/db'
import { restoreElements } from '@excalidraw/excalidraw'
import { useShallow } from 'zustand/react/shallow'
import logger from '../utils/logger'
import { computeElementVersionHash, mergeSceneElements } from '../utils/syncSceneData'
import { getLocalBoardDataPolicy } from '../utils/localBoardData'
import { sanitizeAppStateForSync } from '../utils/sanitizeAppState'
import { fetchWhiteboardSnapshot } from '../utils/fetchWhiteboardSnapshot'
import type { WhiteboardSnapshot } from '../utils/fetchWhiteboardSnapshot'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState, BinaryFiles, ExcalidrawInitialDataState } from '@excalidraw/excalidraw/types'

const DEFAULT_INITIAL_DATA: ExcalidrawInitialDataState = {
	elements: [],
	files: {},
	appState: {},
	scrollToContent: false,
}

export interface UseBoardDataManagerConfig {
	apiBase: string
}

export function useBoardDataManager({ apiBase }: UseBoardDataManagerConfig) {
	const [isLoading, setIsLoading] = useState(true)
	const loadingTimeoutsRef = useRef<Set<ReturnType<typeof setTimeout>>>(new Set())
	const currentFileIdRef = useRef<string | null>(null)

	const { fileId, resolveInitialData, resetInitialDataPromise } = useWhiteboardConfigStore(useShallow(state => ({
		fileId: state.fileId,
		resolveInitialData: state.resolveInitialData,
		resetInitialDataPromise: state.resetInitialDataPromise,
	})))

	// Cleanup function to cancel all pending timeouts
	const cancelPendingTimeouts = useCallback(() => {
		loadingTimeoutsRef.current.forEach(timeout => clearTimeout(timeout))
		loadingTimeoutsRef.current.clear()
	}, [])

	const loadBoard = useCallback(async () => {
		if (!fileId) {
			logger.warn('[BoardDataManager] No fileId provided, cannot load data')
			resolveInitialData(DEFAULT_INITIAL_DATA)
			setIsLoading(false)
			return
		}

		// Store the current fileId to validate later
		currentFileIdRef.current = fileId

		const resolveAfterDelay = (data: ExcalidrawInitialDataState, confirmed = true) => {
			// A short settle delay, so the Excalidraw component has finished
			// mounting by the time it receives the first resolveInitialData.
			const timeout = setTimeout(() => {
				if (currentFileIdRef.current === fileId) {
					resolveInitialData(data, confirmed)
					setIsLoading(false)
				}
				loadingTimeoutsRef.current.delete(timeout)
			}, 50)
			loadingTimeoutsRef.current.add(timeout)
		}

		try {
			const localData = await db.get(fileId)
			const hasPendingLocalChanges = localData?.hasPendingLocalChanges ?? false

			if (currentFileIdRef.current !== fileId) {
				return
			}

			const token = useSessionStore.getState().getToken()
			// A failed fetch (network error, non-404 status, malformed body) is
			// not the same as a confirmed-empty board (a 404): the caller must
			// know which happened, so it never mistakes "the server is
			// unreachable" for "this board is genuinely empty" (see
			// getLocalBoardDataPolicy's 'unavailable' vs 'ignore').
			let serverSnapshot: WhiteboardSnapshot | null = null
			let serverFetchFailed = false
			if (token) {
				try {
					serverSnapshot = await fetchWhiteboardSnapshot(apiBase, token, fileId)
				} catch (error) {
					serverFetchFailed = true
					logger.warn('[BoardDataManager] Server snapshot fetch failed, falling back to local data if present:', error)
				}
			}
			const isReadOnly = useSessionStore.getState().isReadOnly
			const hasServerData = Boolean(serverSnapshot)
			const hasLocalData = Boolean(localData && Array.isArray(localData.elements))
			const localBoardDataPolicy = getLocalBoardDataPolicy(
				hasServerData,
				hasLocalData,
				hasPendingLocalChanges,
				isReadOnly,
				serverFetchFailed,
			)

			if (currentFileIdRef.current !== fileId) {
				return
			}

			let elements: ExcalidrawElement[] = []
			let files: BinaryFiles = {}
			let appState: Partial<AppState> = {}
			// True once a branch below actually resolved a scene (even an empty
			// one, e.g. a freshly created board with 0 elements but a saved
			// appState): only the "neither server nor local had anything usable"
			// case must fall through to DEFAULT_INITIAL_DATA below.
			let dataResolved = false
			// False only when dataResolved stays false because the server fetch
			// itself failed ('unavailable'): the empty scene shown then is not a
			// confirmed board state, and initialDataResolved must stay false so a
			// dedicated syncer never PUTs it back and clobbers the host file. A
			// genuine 404 with no local fallback ('ignore') is a confirmed empty
			// board and keeps this true.
			let resolvedConfirmed = true
			// Set whenever server data was used to resolve the scene: mirrors
			// what the client is about to show back into IndexedDB, so a later
			// load (or the offline path) sees the same state this one resolved.
			let persistAfterLoad: { hasPendingLocalChanges: boolean; lastSyncedHash: number } | null = null

			if (serverSnapshot) {
				const restoredServerElements = restoreElements(serverSnapshot.elements, null)
				const serverHash = computeElementVersionHash(restoredServerElements)
				const sanitizedServerAppState = sanitizeAppStateForSync(serverSnapshot.appState)
				const sanitizedLocalAppState = sanitizeAppStateForSync(localData?.appState)

				if (localData && Array.isArray(localData.elements) && localBoardDataPolicy === 'reconcile') {
					// Local has pending changes – reconcile to avoid losing unsynced work
					const restoredLocalElements = restoreElements(localData.elements, null)
					elements = mergeSceneElements(restoredLocalElements, restoredServerElements, {} as AppState)
					files = { ...localData.files, ...serverSnapshot.files }
					appState = { ...sanitizedLocalAppState, ...sanitizedServerAppState }
					persistAfterLoad = { hasPendingLocalChanges: true, lastSyncedHash: serverHash }
				} else {
					// Use server content as source of truth (restores, clean loads, etc.)
					elements = restoredServerElements
					files = serverSnapshot.files || {}
					appState = { ...sanitizedLocalAppState, ...sanitizedServerAppState }
					persistAfterLoad = { hasPendingLocalChanges: false, lastSyncedHash: serverHash }
				}
				dataResolved = true
			} else if (localData && Array.isArray(localData.elements) && localBoardDataPolicy === 'fallback') {
				// Only writable local data is available
				elements = localData.elements
				files = localData.files || {}
				appState = sanitizeAppStateForSync(localData.appState)
				dataResolved = true
			} else if (localBoardDataPolicy === 'unavailable') {
				// dataResolved stays false and persistAfterLoad stays null: an empty
				// scene is shown, but never persisted as the confirmed board state.
				resolvedConfirmed = false
				logger.warn('[BoardDataManager] Server snapshot unavailable and no local data to fall back on; showing an empty scene')
			}

			if (persistAfterLoad) {
				await db.put(fileId, elements, files, appState as AppState, persistAfterLoad)
			}

			if (currentFileIdRef.current !== fileId) {
				return
			}

			if (dataResolved) {
				const defaultSettings = {
					currentItemFontFamily: 3,
					currentItemStrokeWidth: 1,
					currentItemRoughness: 0,
				}

				resolveAfterDelay({
					elements,
					appState: { ...defaultSettings, ...appState },
					files,
					scrollToContent: elements.length > 0,
				})
			} else {
				resolveAfterDelay(DEFAULT_INITIAL_DATA, resolvedConfirmed)
			}
		} catch (error) {
			logger.error('[BoardDataManager] Error loading data:', error)
			// A failure caught here (a thrown error mid-load, not the handled
			// 'unavailable' policy above) is equally unconfirmed.
			resolveAfterDelay(DEFAULT_INITIAL_DATA, false)
		}
	}, [fileId, resolveInitialData, apiBase])

	// Fires the final SYNC_TO_LOCAL and, once it settles (LOCAL_SYNC_COMPLETE/
	// ERROR, or a 500ms timeout, whichever is first), calls onSettled exactly
	// once. App.tsx terminates the worker from onSettled, not before: the
	// worker must still be alive while this save is in flight.
	const saveOnUnmount = useCallback((onSettled: () => void = () => {}) => {
		const api = useExcalidrawStore.getState().excalidrawAPI
		const currentIsReadOnly = useSessionStore.getState().isReadOnly

		if (!api || currentIsReadOnly) {
			onSettled()
			return
		}

		const currentFileId = useWhiteboardConfigStore.getState().fileId
		const currentWorker = useSyncStore.getState().worker
		const currentIsWorkerReady = useSyncStore.getState().isWorkerReady

		if (!currentIsWorkerReady || !currentWorker || !currentFileId) {
			onSettled()
			return
		}

		try {
			const elements = api.getSceneElementsIncludingDeleted()
			const appState = api.getAppState()
			const files = api.getFiles()
			const filteredAppState = sanitizeAppStateForSync(appState)

			let settled = false
			const settleOnce = () => {
				if (settled) return
				settled = true
				currentWorker.removeEventListener('message', messageHandler)
				clearTimeout(fallbackTimeout)
				onSettled()
			}

			const messageHandler = (event: MessageEvent) => {
				if (event.data.type === 'LOCAL_SYNC_COMPLETE') {
					settleOnce()
				} else if (event.data.type === 'LOCAL_SYNC_ERROR') {
					logger.error('[BoardDataManager] Final sync failed:', event.data.error)
					settleOnce()
				}
			}

			currentWorker.addEventListener('message', messageHandler)

			currentWorker.postMessage({
				type: 'SYNC_TO_LOCAL',
				fileId: currentFileId,
				elements,
				files,
				appState: filteredAppState,
			})

			const fallbackTimeout = setTimeout(settleOnce, 500)
		} catch (error) {
			logger.error('[BoardDataManager] Error during final sync on unmount:', error)
			onSettled()
		}
	}, [])

	// Load data when fileId changes
	useEffect(() => {
		if (!fileId) {
			return
		}

		cancelPendingTimeouts()
		resetInitialDataPromise()

		const api = useExcalidrawStore.getState().excalidrawAPI
		if (api) {
			api.resetScene()
		}

		setIsLoading(true)
		loadBoard()
	}, [fileId, loadBoard, cancelPendingTimeouts, resetInitialDataPromise])

	// Cleanup on unmount
	useEffect(() => {
		return () => {
			cancelPendingTimeouts()
		}
	}, [cancelPendingTimeouts])

	return {
		isLoading,
		loadBoard,
		saveOnUnmount,
	}
}
