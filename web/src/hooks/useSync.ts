/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// Read-only gate: useSessionStore.isReadOnly (bootstrap-immutable from the
// JWT canWrite claim), not useWhiteboardConfigStore.isReadOnly.
//
// Server sync target: `${apiBase}/board` (this project's board REST API,
// internal/boardapi). apiBase comes in as a hook parameter, since it lives
// on AppConfig, not on any store.
//
// Token: useSessionStore.getToken(), synchronous. doSyncViewport's userId
// lookup needs no JWT decode: useSessionStore.userId is already the
// decoded claim.
//
// The incremental-vs-full-vs-skip websocket broadcast decision goes
// through utils/syncSceneData.ts's decideSceneBroadcast (a pure function,
// covered by its own vitest suite), and the per-file IMAGE_ADD dedup hash
// is utils/hashFileContent.ts (same reason).
//
// The final synchronous-XHR flush reads the cached JWT off useSessionStore
// instead of a per-fileId token map: this project mints one session JWT
// per launch.

import { useCallback, useEffect, useMemo, useRef } from 'react'
import { throttle } from 'lodash-es'
import { useWhiteboardConfigStore } from '../stores/useWhiteboardConfigStore'
import { useSyncStore, logSyncResult } from '../stores/useSyncStore'
import { useExcalidrawStore } from '../stores/useExcalidrawStore'
import { useSessionStore } from '../stores/useSessionStore'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import { useShallow } from 'zustand/react/shallow'
import logger from '../utils/logger'
import { sanitizeAppStateForSync } from '../utils/sanitizeAppState'
import { hashFileContent } from '../utils/hashFileContent'
import { estimateDataUrlBytes } from '../utils/imageSizeLimit'
import { withRoomParam } from '../utils/roomParam'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { BinaryFiles } from '@excalidraw/excalidraw/types'
import type { WorkerInboundMessage } from '../types/protocol'
import {
	buildBroadcastedElementVersions,
	computeElementVersionHash,
	decideSceneBroadcast,
	updateBroadcastedElementVersions,
} from '../utils/syncSceneData'
import type { BroadcastedElementVersions } from '../utils/syncSceneData'

// Payload type tags. The socket.io event name itself (the channel these
// payloads travel over) is always the plain string 'server-broadcast' or
// 'server-volatile-broadcast': CollaborationSocket types those against
// ClientToServerEvents' literal keys, which a string enum member is not
// assignable to.
enum SyncMessageType {
	SceneInit = 'SCENE_INIT',
	SceneUpdate = 'SCENE_UPDATE',
	ImageAdd = 'IMAGE_ADD',
	MouseLocation = 'MOUSE_LOCATION',
}

/**
 * Pure gate for doFinalServerSync's beforeunload PUT, factored out so it is
 * testable without mounting a hook (this project has no
 * @testing-library/react dependency; ConflictBanner.tsx's
 * getConflictBannerState sets the same precedent). `reloading` true means
 * this tab already committed to a page reload (ConflictBanner's Reload
 * button or the reload-required broadcast): the server dropped this room's
 * retained scene for that reload, and this PUT would just race the
 * reloaded page's own GET to put a stale scene right back. `initialDataResolved`
 * false means the board's own load has not resolved yet: a PUT before that
 * would send the still-unhydrated, empty scene as the confirmed board state.
 */
export function shouldSkipFinalServerSync(
	fileId: string | null,
	isDedicatedSyncer: boolean,
	conflict: boolean,
	reloading: boolean,
	initialDataResolved: boolean,
): boolean {
	return !fileId || !isDedicatedSyncer || conflict || reloading || !initialDataResolved
}

/**
 * Pure gate for doSyncToLocal's IndexedDB write, factored out so it is
 * testable without mounting a hook (same precedent as
 * shouldSkipFinalServerSync). `reloading` true means this tab already
 * committed to a page reload: a write here would re-persist the just-
 * discarded scene into IndexedDB, and the reloaded page's own load would
 * then reconcile that stale record back into the fresh scene.
 */
export function shouldSkipLocalSync(
	isWorkerReady: boolean,
	hasWorker: boolean,
	fileId: string | null,
	hasExcalidrawAPI: boolean,
	isReadOnly: boolean,
	reloading: boolean,
): boolean {
	return !isWorkerReady || !hasWorker || !fileId || !hasExcalidrawAPI || isReadOnly || reloading
}

/**
 * Options for {@link shouldSkipServerAPISync}, grouped into one object
 * instead of positional booleans: eight of the eleven fields below are
 * `boolean`, and a transposition between two positional booleans is
 * invisible to the type checker.
 */
export interface ServerAPISyncGateOptions {
	forceSync: boolean
	isWorkerReady: boolean
	hasWorker: boolean
	fileId: string | null
	hasExcalidrawAPI: boolean
	isDedicatedSyncer: boolean
	isReadOnly: boolean
	collabStatus: string
	conflict: boolean
	reloading: boolean
	initialDataResolved: boolean
}

/**
 * Pure gate for doSyncToServerAPI's PUT, factored out so it is testable
 * without mounting a hook (same precedent as shouldSkipFinalServerSync).
 * `reloading` true means this tab already committed to a page reload: this
 * PUT would just re-post the stale scene in the window before the reload
 * replaces the page. `forceSync` relaxes the dedicated-syncer and
 * collabStatus checks, for a caller that already runs off the dedicated
 * syncer's own unload handlers; no caller passes it true today
 * (doSyncToServerAPI always calls with the default `false`), so this
 * branch exists only to keep the function's API shape.
 */
export function shouldSkipServerAPISync(options: ServerAPISyncGateOptions): boolean {
	const {
		forceSync, isWorkerReady, hasWorker, fileId, hasExcalidrawAPI,
		isDedicatedSyncer, isReadOnly, collabStatus, conflict, reloading, initialDataResolved,
	} = options
	const baseSkip = !isWorkerReady || !hasWorker || !fileId || !hasExcalidrawAPI || isReadOnly || conflict || reloading || !initialDataResolved
	if (forceSync) {
		return baseSkip
	}
	return baseSkip || !isDedicatedSyncer || collabStatus !== 'online'
}

const LOCAL_SYNC_DELAY = 200
const SERVER_API_SYNC_DELAY = 10000
const WEBSOCKET_SYNC_DELAY = 500
const FULL_SCENE_HEALING_INTERVAL = 20000
const CURSOR_SYNC_DELAY = 50

export interface UseSyncConfig {
	apiBase: string
	maxImageBytes: number
}

export function useSync({ apiBase, maxImageBytes }: UseSyncConfig) {
	const fileId = useWhiteboardConfigStore(state => state.fileId)
	const isReadOnly = useSessionStore(state => state.isReadOnly)

	const {
		initializeWorker,
		isWorkerReady,
		worker,
	} = useSyncStore(
		useShallow(state => ({
			initializeWorker: state.initializeWorker,
			isWorkerReady: state.isWorkerReady,
			worker: state.worker,
		})),
	)

	const { excalidrawAPI } = useExcalidrawStore(
		useShallow(state => ({
			excalidrawAPI: state.excalidrawAPI,
		})),
	)

	const { isDedicatedSyncer, status: collabStatus, socket, isInRoom, conflict } = useCollaborationStore(
		useShallow(state => ({
			isDedicatedSyncer: state.isDedicatedSyncer,
			status: state.status,
			socket: state.socket,
			isInRoom: state.isInRoom,
			conflict: state.conflict,
		})),
	)

	useEffect(() => {
		initializeWorker()
		// App's own unmount effect terminates the worker, after its final
		// save-on-unmount completes (see App.tsx and useBoardDataManager's
		// saveOnUnmount): terminating it here too would race that save, since
		// this effect's cleanup runs before App's.
	}, [initializeWorker])

	// Keep track of previously synced files to avoid resending unchanged files
	const prevSyncedFilesRef = useRef<Record<string, string>>({})
	const lastBroadcastedSceneHashRef = useRef<number | null>(null)
	const broadcastedElementVersionsRef = useRef<BroadcastedElementVersions>({})
	const hasBroadcastedSceneRef = useRef(false)

	// Reset prevSyncedFilesRef when fileId changes to prevent leakage across files
	useEffect(() => {
		prevSyncedFilesRef.current = {}
		lastBroadcastedSceneHashRef.current = null
		broadcastedElementVersionsRef.current = {}
		hasBroadcastedSceneRef.current = false
	}, [fileId])

	useEffect(() => {
		lastBroadcastedSceneHashRef.current = null
		broadcastedElementVersionsRef.current = {}
		hasBroadcastedSceneRef.current = false
	}, [socket?.id])

	useEffect(() => {
		if (!isInRoom) {
			lastBroadcastedSceneHashRef.current = null
			broadcastedElementVersionsRef.current = {}
			hasBroadcastedSceneRef.current = false
		}
	}, [isInRoom])

	// Saves current state to IndexedDB
	const doSyncToLocal = useCallback(async () => {
		// A tab that already committed to a reload must not re-persist the
		// discarded scene locally: see shouldSkipLocalSync.
		const reloading = useCollaborationStore.getState().reloading
		if (shouldSkipLocalSync(isWorkerReady, !!worker, fileId, !!excalidrawAPI, isReadOnly, reloading)) {
			return
		}

		// shouldSkipLocalSync already ruled out a missing worker/excalidrawAPI;
		// narrow explicitly since TS cannot see through the extracted gate.
		if (!worker || !excalidrawAPI) {
			return
		}

		try {
			const elements = excalidrawAPI.getSceneElementsIncludingDeleted() as readonly ExcalidrawElement[]
			const appState = excalidrawAPI.getAppState()
			const files = excalidrawAPI.getFiles() as BinaryFiles
			const filteredAppState = sanitizeAppStateForSync(appState)

			const message: WorkerInboundMessage = { type: 'SYNC_TO_LOCAL', fileId, elements, files, appState: filteredAppState }
			worker.postMessage(message)
			logSyncResult('local', { status: 'syncing' })
		} catch (error) {
			logger.error('[Sync] Local sync failed:', error)
			logSyncResult('local', { status: 'error', error: error instanceof Error ? error.message : String(error) })
		}
	}, [isWorkerReady, worker, fileId, excalidrawAPI, isReadOnly])

	// Saves current state to the server's board API
	const doSyncToServerAPI = useCallback(async (forceSync = false) => {
		logger.debug('[Sync] doSyncToServerAPI called', { forceSync, isDedicatedSyncer, collabStatus })

		// The board's own load must resolve first: a PUT before that would send
		// the still-unhydrated, empty scene as the confirmed board state (the
		// race useWhiteboardConfigStore's initialDataResolved flag exists to
		// close). Read the store directly rather than a hook so this gate
		// stays current even under the throttle's leading-edge call.
		const initialDataResolved = useWhiteboardConfigStore.getState().initialDataResolved
		// A tab that committed to a reload must not re-post its stale scene.
		const reloading = useCollaborationStore.getState().reloading

		// One gate call for both the normal and the force-sync path: forceSync
		// itself already selects which checks shouldSkipServerAPISync applies
		// (see its own doc comment). The conflict gate applies to both: the
		// server already pauses a room's save loop while it is in conflict
		// (internal/room's saveDueLocked), so a PUT here would just be
		// stored and never reach the WOPI host — skipping it client-side avoids
		// hammering the endpoint for no effect.
		if (shouldSkipServerAPISync({
			forceSync, isWorkerReady, hasWorker: !!worker, fileId, hasExcalidrawAPI: !!excalidrawAPI,
			isDedicatedSyncer, isReadOnly, collabStatus, conflict, reloading, initialDataResolved,
		})) {
			logger.debug('[Sync] Skipping server sync', {
				forceSync, isWorkerReady, worker: !!worker, fileId, excalidrawAPI: !!excalidrawAPI, isDedicatedSyncer, isReadOnly, collabStatus, conflict, reloading, initialDataResolved,
			})
			return
		}

		// Both guards above already ruled out a missing worker/excalidrawAPI for
		// every remaining path; narrow explicitly since TS cannot merge two
		// separate early-return conditions into one flow fact.
		if (!worker || !excalidrawAPI) {
			return
		}

		logSyncResult('server', { status: 'syncing API' })
		logger.debug('[Sync] Sending SYNC_TO_SERVER message to worker')

		try {
			const jwt = useSessionStore.getState().getToken()
			if (!jwt) throw new Error('JWT token missing for server API sync.')
			if (useWhiteboardConfigStore.getState().fileId !== fileId) {
				throw new Error('FileId changed during server sync preparation.')
			}

			const elements = excalidrawAPI.getSceneElementsIncludingDeleted() as readonly ExcalidrawElement[]
			const files = excalidrawAPI.getFiles() as BinaryFiles

			const message: WorkerInboundMessage = {
				type: 'SYNC_TO_SERVER',
				fileId,
				url: withRoomParam(`${apiBase}/board`, fileId),
				jwt,
				elements,
				files,
			}

			worker.postMessage(message)
			logger.debug('[Sync] SYNC_TO_SERVER message sent to worker')
		} catch (error) {
			logger.error('[Sync] Server API sync failed:', error)
			logSyncResult('server', { status: 'error API', error: error instanceof Error ? error.message : String(error) })
		}
	}, [isWorkerReady, worker, fileId, excalidrawAPI, isDedicatedSyncer, isReadOnly, collabStatus, conflict, apiBase])

	// Syncs scene and files via WebSocket
	const doSyncViaWebSocket = useCallback(async () => {
		if (!fileId || !excalidrawAPI || !socket || collabStatus !== 'online' || isReadOnly || !isInRoom) {
			return
		}

		try {
			const elements = excalidrawAPI.getSceneElementsIncludingDeleted() as readonly ExcalidrawElement[]
			const files = excalidrawAPI.getFiles() as BinaryFiles
			let syncedElementsCount = 0

			const decision = decideSceneBroadcast(
				elements,
				hasBroadcastedSceneRef.current,
				lastBroadcastedSceneHashRef.current,
				broadcastedElementVersionsRef.current,
			)

			if (decision.kind === 'init') {
				const sceneData = { type: SyncMessageType.SceneInit, payload: { elements: decision.elements } }
				const sceneBuffer = new TextEncoder().encode(JSON.stringify(sceneData))
				socket.emit('server-broadcast', fileId, sceneBuffer, [])
				hasBroadcastedSceneRef.current = true
				lastBroadcastedSceneHashRef.current = decision.hash
				broadcastedElementVersionsRef.current = buildBroadcastedElementVersions(elements)
				syncedElementsCount = decision.elements.length
			} else if (decision.kind === 'update') {
				const sceneData = { type: SyncMessageType.SceneUpdate, payload: { elements: decision.elements } }
				const sceneBuffer = new TextEncoder().encode(JSON.stringify(sceneData))
				socket.emit('server-broadcast', fileId, sceneBuffer, [])
				lastBroadcastedSceneHashRef.current = decision.hash
				broadcastedElementVersionsRef.current = updateBroadcastedElementVersions(
					broadcastedElementVersionsRef.current,
					decision.elements,
				)
				syncedElementsCount = decision.elements.length
			}

			// Send only new or changed files
			if (files && Object.keys(files).length > 0) {
				const currentFileHashes: Record<string, string> = {}
				for (const fileIdKey in files) {
					const file = files[fileIdKey]
					if (!file?.dataURL) continue

					const byteSize = estimateDataUrlBytes(file.dataURL)
					if (byteSize > maxImageBytes) {
						logger.warn(`[Sync] Skipping oversized image ${fileIdKey}: ${byteSize} bytes exceeds the ${maxImageBytes} byte limit`)
						continue
					}

					const currentHash = hashFileContent(file.dataURL)
					currentFileHashes[fileIdKey] = currentHash
					if (prevSyncedFilesRef.current[fileIdKey] !== currentHash) {
						const fileData = { type: SyncMessageType.ImageAdd, payload: { file } }
						const fileJson = JSON.stringify(fileData)
						const fileBuffer = new TextEncoder().encode(fileJson)
						socket.emit('server-broadcast', fileId, fileBuffer, [])
					}
				}
				prevSyncedFilesRef.current = currentFileHashes
				logSyncResult('websocket', { status: 'sync success', elementsCount: syncedElementsCount })
			} else {
				logSyncResult('websocket', { status: 'sync success', elementsCount: syncedElementsCount })
				prevSyncedFilesRef.current = {}
			}
		} catch (error) {
			logger.error('[Sync] WebSocket sync failed:', error)
			logSyncResult('websocket', { status: 'sync error', error: error instanceof Error ? error.message : String(error) })
		}
	}, [fileId, excalidrawAPI, socket, collabStatus, isReadOnly, isInRoom, maxImageBytes])

	const doPeriodicFullSceneHealing = useCallback(() => {
		if (!fileId || !excalidrawAPI || !socket || collabStatus !== 'online' || isReadOnly || !isInRoom || !hasBroadcastedSceneRef.current) {
			return
		}

		try {
			const elements = excalidrawAPI.getSceneElementsIncludingDeleted() as readonly ExcalidrawElement[]
			const sceneHash = computeElementVersionHash(elements)
			const sceneData = { type: SyncMessageType.SceneUpdate, payload: { elements } }
			const sceneBuffer = new TextEncoder().encode(JSON.stringify(sceneData))
			socket.emit('server-broadcast', fileId, sceneBuffer, [])
			lastBroadcastedSceneHashRef.current = sceneHash
			broadcastedElementVersionsRef.current = buildBroadcastedElementVersions(elements)
		} catch (error) {
			logger.error('[Sync] Periodic full-scene healing failed:', error)
		}
	}, [fileId, excalidrawAPI, socket, collabStatus, isReadOnly, isInRoom])

	// Each ref holds the latest version of one callback. The throttle
	// wrappers below read through the ref, so useMemo can create each one
	// exactly once. A lodash throttle recreated on every callback change
	// loses its throttle window and any pending trailing call.
	const doSyncToLocalRef = useRef(doSyncToLocal)
	const doSyncToServerAPIRef = useRef(doSyncToServerAPI)
	const doSyncViaWebSocketRef = useRef(doSyncViaWebSocket)
	const doPeriodicFullSceneHealingRef = useRef(doPeriodicFullSceneHealing)

	useEffect(() => {
		doSyncToLocalRef.current = doSyncToLocal
		doSyncToServerAPIRef.current = doSyncToServerAPI
		doSyncViaWebSocketRef.current = doSyncViaWebSocket
		doPeriodicFullSceneHealingRef.current = doPeriodicFullSceneHealing
	})

	const throttledSyncToLocal = useMemo(() =>
		throttle(() => doSyncToLocalRef.current(), LOCAL_SYNC_DELAY, { leading: true, trailing: true })
	, [])

	const throttledSyncToServerAPI = useMemo(() =>
		throttle((forceSync?: boolean) => doSyncToServerAPIRef.current(forceSync), SERVER_API_SYNC_DELAY, { leading: true, trailing: true })
	, [])

	const throttledSyncViaWebSocket = useMemo(() =>
		throttle(() => doSyncViaWebSocketRef.current(), WEBSOCKET_SYNC_DELAY, { leading: true, trailing: true })
	, [])

	const throttledFullSceneHealing = useMemo(() =>
		throttle(() => doPeriodicFullSceneHealingRef.current(), FULL_SCENE_HEALING_INTERVAL, { leading: false, trailing: true })
	, [])

	const flushPendingWebSocketSync = useCallback(() => {
		if (!fileId || !excalidrawAPI || !socket || collabStatus !== 'online' || isReadOnly || !isInRoom) {
			return
		}

		throttledSyncViaWebSocket.flush()
	}, [fileId, excalidrawAPI, socket, collabStatus, isReadOnly, isInRoom, throttledSyncViaWebSocket])

	useEffect(() => {
		if (isInRoom && fileId && socket && collabStatus === 'online' && !isReadOnly) {
			throttledSyncViaWebSocket()
		}
	}, [isInRoom, fileId, socket, collabStatus, isReadOnly, throttledSyncViaWebSocket])

	const doSyncCursors = useCallback(
		(payload: {
			pointer: { x: number; y: number; tool: 'pointer' | 'laser' }
			button: 'down' | 'up'
		}) => {
			if (!fileId || !excalidrawAPI || !socket || collabStatus !== 'online') {
				return
			}

			try {
				const data = {
					type: SyncMessageType.MouseLocation,
					payload: {
						pointer: payload.pointer,
						button: payload.button,
						selectedElementIds: excalidrawAPI.getAppState().selectedElementIds,
					},
				}
				const json = JSON.stringify(data)
				const encodedBuffer = new TextEncoder().encode(json)
				socket.emit('server-volatile-broadcast', fileId, encodedBuffer)
				logSyncResult('cursor', { status: 'sync success' })
			} catch (error) {
				logger.error('[Sync] Error syncing cursor:', error)
				logSyncResult('cursor', { status: 'sync error', error: error instanceof Error ? error.message : String(error) })
			}
		},
		[fileId, excalidrawAPI, socket, collabStatus],
	)

	const doSyncCursorsRef = useRef(doSyncCursors)
	useEffect(() => {
		doSyncCursorsRef.current = doSyncCursors
	})

	const throttledSyncCursors = useMemo(() =>
		throttle(
			(payload: { pointer: { x: number; y: number; tool: 'pointer' | 'laser' }; button: 'down' | 'up' }) =>
				doSyncCursorsRef.current(payload),
			CURSOR_SYNC_DELAY,
			{ leading: true, trailing: true },
		)
	, [])

	const onChange = useCallback(() => {
		// Update cached state immediately on every change
		if (excalidrawAPI) {
			const elements = excalidrawAPI.getSceneElementsIncludingDeleted()
			const files = excalidrawAPI.getFiles()
			cachedStateRef.current = { elements, files }
			const sceneHash = computeElementVersionHash(elements)
			if (sceneHash !== lastBroadcastedSceneHashRef.current) {
				throttledFullSceneHealing()
			}
		}

		throttledSyncToLocal()
		throttledSyncToServerAPI()
		throttledSyncViaWebSocket()

		logger.debug('[Sync] Changes detected, triggered sync operations')
	}, [throttledSyncToLocal, throttledSyncToServerAPI, throttledSyncViaWebSocket, throttledFullSceneHealing, excalidrawAPI])

	const onPointerUpdate = useCallback(
		(payload: {
			pointersMap: Map<number, { x: number; y: number }>,
			pointer: { x: number; y: number; tool: 'laser' | 'pointer' },
			button: 'down' | 'up'
		}) => {
			if (payload.pointersMap.size < 2) {
				throttledSyncCursors({ pointer: payload.pointer, button: payload.button })
			}
		},
		[throttledSyncCursors],
	)

	// Capture syncer state immediately to avoid closure issues
	const isSyncerRef = useRef(isDedicatedSyncer)
	useEffect(() => {
		isSyncerRef.current = isDedicatedSyncer
	}, [isDedicatedSyncer])

	// Cache the latest state for final sync - update on EVERY change
	const cachedStateRef = useRef<{ elements: readonly ExcalidrawElement[]; files: BinaryFiles }>({ elements: [], files: {} as BinaryFiles })

	// Direct sync when leaving - synchronous to ensure it completes
	const doFinalServerSync = useCallback(() => {
		// reloading and initialDataResolved read the store directly instead
		// of through a hook, so this callback's identity does not change on
		// every flip.
		if (shouldSkipFinalServerSync(
			fileId,
			isSyncerRef.current,
			conflict,
			useCollaborationStore.getState().reloading,
			useWhiteboardConfigStore.getState().initialDataResolved,
		)) {
			return
		}

		logger.debug('[Sync] Executing final sync on page leave')

		try {
			const jwt = useSessionStore.getState().getToken()
			if (!jwt) {
				return
			}

			// The live scene may already be torn down by the time this runs; read the cache instead.
			const { elements, files } = cachedStateRef.current
			logger.debug('[Sync] Using cached state with', elements.length, 'elements')

			const url = withRoomParam(`${apiBase}/board`, fileId)
			const data = JSON.stringify({ elements, files: files || {} })

			// Use synchronous XMLHttpRequest (works in beforeunload)
			const xhr = new XMLHttpRequest()
			xhr.open('PUT', url, false) // false = synchronous
			xhr.setRequestHeader('Content-Type', 'application/json')
			xhr.setRequestHeader('Authorization', `Bearer ${jwt}`)

			xhr.send(data)
		} catch (error) {
			logger.error('[Sync] Final sync failed:', error)
		}
	}, [fileId, apiBase, conflict])

	// excalidrawAPI/isReadOnly/flushPendingWebSocketSync/doSyncToLocal/
	// doFinalServerSync each change identity often during a session. The two
	// effects below need the latest version of each, but must not re-run on
	// every such change themselves. So this effect runs on every
	// render, with no cleanup, and just refreshes the refs.
	const excalidrawAPIRef = useRef(excalidrawAPI)
	const isReadOnlyRef = useRef(isReadOnly)
	const flushPendingWebSocketSyncRef = useRef(flushPendingWebSocketSync)
	const doSyncToLocalForFlushRef = useRef(doSyncToLocal)
	const doFinalServerSyncRef = useRef(doFinalServerSync)

	useEffect(() => {
		excalidrawAPIRef.current = excalidrawAPI
		isReadOnlyRef.current = isReadOnly
		flushPendingWebSocketSyncRef.current = flushPendingWebSocketSync
		doSyncToLocalForFlushRef.current = doSyncToLocal
		doFinalServerSyncRef.current = doFinalServerSync
	})

	// Registers the beforeunload/visibilitychange listeners once.
	// throttledSyncToLocal and throttledSyncToServerAPI are stable for the
	// component's lifetime (see above), so this dependency array never
	// changes. The listeners are added and removed exactly once.
	useEffect(() => {
		const handleBeforeUnload = () => {
			const api = excalidrawAPIRef.current
			const readOnly = isReadOnlyRef.current

			if (api && !readOnly) {
				flushPendingWebSocketSyncRef.current()
			}

			if (api && !readOnly && isSyncerRef.current) {
				// Cancel any pending throttled trailing call FIRST
				throttledSyncToLocal.cancel()
				throttledSyncToServerAPI.cancel()
				// Call the unthrottled versions directly
				doSyncToLocalForFlushRef.current()
				doFinalServerSyncRef.current()
			}
		}

		// Also handle visibility change as backup for mobile/tabs
		const handleVisibilityChange = () => {
			if (document.visibilityState !== 'hidden' || !excalidrawAPIRef.current || isReadOnlyRef.current) {
				return
			}

			flushPendingWebSocketSyncRef.current()

			if (isSyncerRef.current) {
				throttledSyncToLocal.cancel()
				throttledSyncToServerAPI.cancel()
				doSyncToLocalForFlushRef.current()
				doFinalServerSyncRef.current()
			}
		}

		window.addEventListener('beforeunload', handleBeforeUnload)
		document.addEventListener('visibilitychange', handleVisibilityChange)

		return () => {
			window.removeEventListener('beforeunload', handleBeforeUnload)
			document.removeEventListener('visibilitychange', handleVisibilityChange)
		}
	}, [throttledSyncToLocal, throttledSyncToServerAPI])

	// Unmount-only final flush. This does not call doSyncToLocal: App's
	// own unmount effect already runs an equivalent final local save
	// through saveOnUnmount, and that path waits
	// for the worker to actually settle before terminating it. This effect
	// only still owns the final server-side flush and the websocket/throttle
	// teardown. Every dependency here (the five throttle instances) is
	// stable for the component's lifetime, so this effect too only fires
	// once, on the real unmount.
	useEffect(() => {
		return () => {
			const api = excalidrawAPIRef.current
			const readOnly = isReadOnlyRef.current

			if (api && !readOnly) {
				flushPendingWebSocketSyncRef.current()
			}

			if (isSyncerRef.current && api && !readOnly) {
				throttledSyncToServerAPI.cancel()
				doFinalServerSyncRef.current()
			}

			// Cancel all throttled functions to prevent them from running after unmount
			throttledSyncToLocal.cancel()
			throttledSyncToServerAPI.cancel()
			throttledSyncViaWebSocket.cancel()
			throttledFullSceneHealing.cancel()
			throttledSyncCursors.cancel()
		}
	}, [throttledSyncToLocal, throttledSyncToServerAPI, throttledSyncViaWebSocket, throttledFullSceneHealing, throttledSyncCursors])

	return { onChange, onPointerUpdate }
}
