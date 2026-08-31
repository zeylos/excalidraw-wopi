/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// Connect target: `io('/', { path, ... })` — the page origin, using this
// project's socketPath config (this project has no per-board setting; the
// relay always lives at the same origin as the editor page).
//
// connectSocket is fully synchronous (no async function body): token
// retrieval is synchronous (see below), and `Socket#disconnect()` tears
// the client instance down immediately (it does not wait on the network),
// so nothing is left to await.
//
// Auth: useSessionStore.getToken(), synchronous, one JWT per launch. There
// is no token-refresh path: the session JWT is minted once, at launch,
// with no refresh, so a persistent auth failure is terminal — the circuit
// breaker (useCollaborationStore) is the only response.
//
// Presence and cursor are two separate channels here, matching the Go
// relay (internal/relay): `room-user-change` carries only roster identity
// (RoomPresenceEntry: socketId/user/userId/socketIds, no pointer —
// internal/relay/relay.go's presenceEntry), while live pointer travels
// only through a MOUSE_LOCATION client-broadcast, whose `user` field the
// relay itself stamps with the sender's identity
// (internal/relay/broadcast.go's rewriteVolatile).
//
// Self-filtering uses useSessionStore.userId directly (no async JWT
// decode) since it is already the decoded, trusted claim.
//
// client-broadcast decode/validate is the pure, tested
// utils/decodeClientBroadcast.ts; this hook only holds the dispatch
// switch.
//
// This hook does not implement: clientType/isRecording (recording),
// request-presenter-viewport / send-viewport-request /
// presenter-viewport-update (presentation), isVersionPreview gating (no
// version-preview feature here), and VIEWPORT_UPDATE/SCENE_RESTORE
// (dead code: nothing emits either — viewport following was a
// presentation-mode feature this project never has, and the live reload
// mechanism is the `reload-required` socket event).
//
// handleClientBroadcast's "skip an identical repeat scene payload" dedup
// lives in a useRef rather than a plain `let` local: a plain `let` resets
// to null on every render, so the dedup would only ever compare against
// `null` — never a genuine repeat.
//
// The four reconnection events (reconnect/reconnect_attempt/
// reconnect_error/reconnect_failed) are registered on `socketInstance.io`
// (the Manager), not the socket itself. socket.io-client v4 only reserves
// connect/connect_error/disconnect on the Socket type (socket.d.ts's
// SocketReservedEvents) and never forwards Manager reconnection events
// onto the socket at runtime, so registering them on the socket directly
// would compile but never fire.
//
// perMessageDeflate drops zlibDeflateOptions/zlibInflateOptions: those are
// Node `ws`-server options with no browser-client equivalent (the browser
// WebSocket API has no per-message-deflate tuning surface); only
// `threshold`, which the browser engine.io-client type actually declares,
// is kept.

import { useCallback, useEffect, useMemo, useRef } from 'react'
import type {
	AppState,
	BinaryFileData,
	BinaryFiles,
	Collaborator,
	SocketId,
} from '@excalidraw/excalidraw/types'
import type { ExcalidrawElement, ExcalidrawImageElement } from '@excalidraw/excalidraw/element/types'
import type { RemoteExcalidrawElement } from '@excalidraw/excalidraw/data/reconcile'
import { restoreElements } from '@excalidraw/excalidraw'
import { mergeElementsWithMetadata } from '../utils/mergeElementsWithMetadata'
import { io } from 'socket.io-client'
import { useExcalidrawStore } from '../stores/useExcalidrawStore'
import { useSessionStore } from '../stores/useSessionStore'
import { useWhiteboardConfigStore } from '../stores/useWhiteboardConfigStore'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import { db } from '../database/db'
import { sanitizeAppStateForSync } from '../utils/sanitizeAppState'
import { useShallow } from 'zustand/react/shallow'
import { throttle, debounce } from 'lodash-es'
import type { DebouncedFunc } from 'lodash-es'
import { classifyConnectError } from '../utils/connectError'
import { decodeClientBroadcast } from '../utils/decodeClientBroadcast'
import { shouldRejectRemoteImage } from '../utils/imageSizeLimit'
import { shouldJoinRoom, shouldRetryJoin } from '../utils/roomJoin'
import {
	createSceneDedupTracker,
	isDuplicateScenePayload,
	markScenePayloadApplied,
	resetSceneDedup,
} from '../utils/sceneDedup'
import logger from '../utils/logger'
import type { CollaboratorPayload, CollaborationSocket, RoomPresenceEntry } from '../types/collaboration'

const CURSOR_UPDATE_DELAY = 50
const JOIN_CONFIRM_TIMEOUT = 3000
const MAX_JOIN_RETRIES = 3
// Membership divergence during a rollout lasts seconds; retrying uncapped
// is fine since each fresh HTTP handshake re-routes through the relay's
// middleware to the file's current owner.
const WRONG_REPLICA_RETRY_DELAY = 3000

export interface UseCollaborationConfig {
	socketPath: string
	maxImageBytes: number
}

export function useCollaboration({ socketPath, maxImageBytes }: UseCollaborationConfig) {
	const joinedRoomRef = useRef<string | null>(null)
	const joinRetryCountRef = useRef(0)
	const joinRetryTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)
	const wrongReplicaRetryTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)
	// Mirrors scheduleWrongReplicaRetry (defined after connectSocket/
	// disconnectSocket, below): setupSocketEventHandlers is defined earlier
	// in this hook and cannot reference those consts directly without a TDZ
	// crash, so its connect_error handler goes through this ref instead.
	const scheduleWrongReplicaRetryRef = useRef<() => void>(() => {})
	const cursorThrottleMapRef = useRef<Map<string, DebouncedFunc<(payload: CollaboratorPayload) => void>>>(new Map())
	const pendingSceneUpdateRef = useRef<readonly ExcalidrawElement[] | null>(null)
	const pendingImageUpdatesRef = useRef<Map<string, BinaryFileData>>(new Map())
	const sceneDedupRef = useRef(createSceneDedupTracker())
	const pendingSceneReplaceRef = useRef<{
		elements: ExcalidrawElement[]
		files: BinaryFiles
		appState: Partial<AppState>
		scrollToContent: boolean
	} | null>(null)

	const { excalidrawAPI } = useExcalidrawStore(
		useShallow(state => ({
			excalidrawAPI: state.excalidrawAPI,
		})),
	)

	const fileId = useWhiteboardConfigStore(state => state.fileId)

	const {
		setStatus,
		setSocket,
		setDedicatedSyncer,
		markTerminalAuthFailure,
		clearAuthError,
		resetStore,
		setIsInRoom,
		setConflict,
		setSaveStalled,
	} = useCollaborationStore(
		useShallow(state => ({
			setStatus: state.setStatus,
			setSocket: state.setSocket,
			setDedicatedSyncer: state.setDedicatedSyncer,
			markTerminalAuthFailure: state.markTerminalAuthFailure,
			clearAuthError: state.clearAuthError,
			resetStore: state.resetStore,
			setIsInRoom: state.setIsInRoom,
			setConflict: state.setConflict,
			setSaveStalled: state.setSaveStalled,
		})),
	)

	const reconcileAndApplyRemoteElements = useCallback(
		(remoteElements: readonly ExcalidrawElement[]) => {
			if (!excalidrawAPI) return

			try {
				const restoredRemoteElements = restoreElements(remoteElements, null)
				const localElements = excalidrawAPI.getSceneElementsIncludingDeleted() || []
				const appState = excalidrawAPI.getAppState()
				const reconciledElements = mergeElementsWithMetadata(
					localElements,
					restoredRemoteElements as RemoteExcalidrawElement[],
					appState,
				)
				excalidrawAPI.updateScene({ elements: reconciledElements })
				markScenePayloadApplied(sceneDedupRef.current, JSON.stringify(remoteElements))

				// Request any missing images
				const currentFiles = excalidrawAPI.getFiles()
				const currentSocket = useCollaborationStore.getState().socket

				if (currentSocket?.connected && fileId) {
					const missingImages = restoredRemoteElements
						.filter((el) => {
							if (el.type !== 'image') return false
							const imageId = (el as unknown as ExcalidrawImageElement).fileId
							return Boolean(imageId) && !currentFiles[imageId as string]
						})

					missingImages.forEach(el => {
						const imageId = (el as unknown as ExcalidrawImageElement).fileId as string
						logger.debug(`[Collaboration] Requesting missing image: ${imageId}`)
						currentSocket.emit('image-get', fileId, imageId)
					})
				}
			} catch (error) {
				logger.error('[Collaboration] Error reconciling remote elements:', error)
			}
		},
		[excalidrawAPI, fileId],
	)

	const handleRemoteImageAdd = useCallback(
		(file: BinaryFileData) => {
			if (!excalidrawAPI) return

			// A malicious or compromised peer could push an oversized image to
			// OOM every other tab in the room. The relay forwards opaque bytes
			// and never parses a single image out of the broadcast, so this is
			// the first point on the receive path that can see a decoded image
			// size at all.
			if (shouldRejectRemoteImage(file, maxImageBytes)) {
				logger.warn(`[Collaboration] Rejecting oversized remote image ${file.id}: exceeds the ${maxImageBytes} byte limit`)
				return
			}

			try {
				const existingFiles = excalidrawAPI.getFiles()
				if (!existingFiles[file.id]) {
					logger.debug(`[Collaboration] Adding received image: ${file.id}`)
					excalidrawAPI.addFiles([file])
				} else {
					logger.debug(`[Collaboration] Image already exists: ${file.id}, skipping`)
				}
			} catch (error) {
				logger.error('[Collaboration] Error processing received image:', error)
			}
		},
		[excalidrawAPI, maxImageBytes],
	)

	const queueSceneUpdate = useCallback(
		(remoteElements: readonly ExcalidrawElement[]) => {
			if (!excalidrawAPI) {
				pendingSceneUpdateRef.current = remoteElements
				return
			}

			reconcileAndApplyRemoteElements(remoteElements)
		},
		[excalidrawAPI, reconcileAndApplyRemoteElements],
	)

	const queueImageUpdate = useCallback(
		(file: BinaryFileData) => {
			if (!excalidrawAPI) {
				pendingImageUpdatesRef.current.set(file.id, file)
				return
			}

			handleRemoteImageAdd(file)
		},
		[excalidrawAPI, handleRemoteImageAdd],
	)

	const applySceneReplacement = useCallback(
		(payload: {
			elements: ExcalidrawElement[]
			files: BinaryFiles
			appState: Partial<AppState>
			scrollToContent: boolean
		}) => {
			if (!excalidrawAPI) {
				return
			}

			try {
				excalidrawAPI.resetScene()

				const currentAppState = excalidrawAPI.getAppState()
				const sanitizedPayloadAppState = sanitizeAppStateForSync(payload.appState)
				const mergedAppState = {
					...currentAppState,
					...sanitizedPayloadAppState,
					scrollToContent: payload.scrollToContent,
				}

				excalidrawAPI.updateScene({
					elements: payload.elements,
					appState: mergedAppState,
				})

				const filesArray = Object.values(payload.files || {}).filter(
					(file): file is BinaryFileData => Boolean(file),
				)

				if (filesArray.length > 0) {
					excalidrawAPI.addFiles(filesArray)
				}
			} catch (error) {
				logger.error('[Collaboration] Error applying restored scene:', error)
			}
		},
		[excalidrawAPI],
	)

	useEffect(() => {
		if (!excalidrawAPI) {
			return
		}

		if (pendingSceneReplaceRef.current) {
			const payload = pendingSceneReplaceRef.current
			pendingSceneReplaceRef.current = null
			applySceneReplacement(payload)
		}

		if (pendingSceneUpdateRef.current) {
			const latestScene = pendingSceneUpdateRef.current
			pendingSceneUpdateRef.current = null
			reconcileAndApplyRemoteElements(latestScene)
		}

		if (pendingImageUpdatesRef.current.size > 0) {
			const pendingImages = Array.from(pendingImageUpdatesRef.current.values())
			pendingImageUpdatesRef.current.clear()
			pendingImages.forEach(image => {
				handleRemoteImageAdd(image)
			})
		}
	}, [excalidrawAPI, handleRemoteImageAdd, reconcileAndApplyRemoteElements, applySceneReplacement])

	useEffect(() => {
		pendingSceneUpdateRef.current = null
		pendingImageUpdatesRef.current.clear()
		pendingSceneReplaceRef.current = null
		resetSceneDedup(sceneDedupRef.current)
	}, [fileId])

	// A new socket (a fresh connection or a reconnect) starts a new session
	// with the relay; a dedup fingerprint from the previous one must not
	// suppress the first broadcast of the new one.
	const socketId = useCollaborationStore(state => state.socket?.id)
	useEffect(() => {
		resetSceneDedup(sceneDedupRef.current)
	}, [socketId])

	// Stock Excalidraw keys its collaborators map with the branded SocketId
	// type. This project keys collaborators by the persistent userId instead
	// (a socket id would churn across reconnects); toCollaboratorKey is the
	// one cast site that bridges the two.
	const toCollaboratorKey = (userId: string) => userId as unknown as SocketId

	// room-user-change carries roster identity only (no pointer); a joiner's
	// or leaver's entry there never touches pointer state, so a still-present
	// user keeps whatever MOUSE_LOCATION last reported for them.
	const updateRoomPresence = useCallback(
		(entries: RoomPresenceEntry[]) => {
			if (!excalidrawAPI) return

			const currentUserId = useSessionStore.getState().userId
			const currentCollaborators = excalidrawAPI.getAppState().collaborators || new Map<SocketId, Collaborator>()
			const nextCollaborators = new Map<SocketId, Collaborator>()

			entries.forEach((entry) => {
				if (entry.user.id === currentUserId) return
				const key = toCollaboratorKey(entry.user.id)
				const existing = currentCollaborators.get(key)
				nextCollaborators.set(key, {
					id: entry.user.id,
					username: entry.user.name,
					pointer: existing?.pointer,
					button: existing?.button,
					selectedElementIds: existing?.selectedElementIds ?? {},
				})
			})

			excalidrawAPI.updateScene({ collaborators: nextCollaborators })
			logger.debug(`[Collaboration] Updated presence: ${nextCollaborators.size} users online (filtered out current user)`)

			// Cancel and drop the per-user cursor throttle for anyone no longer in
			// the roster: without this, a throttled MOUSE_LOCATION already queued
			// for a departed user fires after they left and resurrects their
			// cursor.
			const rosterIds = new Set(entries.map(entry => entry.user.id))
			cursorThrottleMapRef.current.forEach((throttledFn, userId) => {
				if (!rosterIds.has(userId)) {
					throttledFn.cancel()
					cursorThrottleMapRef.current.delete(userId)
				}
			})
		},
		[excalidrawAPI],
	)

	// Function to update cursor state (unthrottled version)
	const doUpdateCursor = useCallback(
		(payload: CollaboratorPayload) => {
			if (!excalidrawAPI) return

			try {
				const currentUserId = useSessionStore.getState().userId
				if (payload.user.id === currentUserId) {
					return
				}

				const currentCollaborators = excalidrawAPI.getAppState().collaborators || new Map<SocketId, Collaborator>()
				const updatedCollaborators = new Map<SocketId, Collaborator>(currentCollaborators)

				updatedCollaborators.set(toCollaboratorKey(payload.user.id), {
					id: payload.user.id,
					username: payload.user.name,
					pointer: payload.pointer,
					button: payload.button,
					// We don't need selectedElementIds for cursor updates
					selectedElementIds: {},
				})

				excalidrawAPI.updateScene({ collaborators: updatedCollaborators })
			} catch (error) {
				logger.error('[Collaboration] Error updating cursor:', error)
			}
		},
		[excalidrawAPI],
	)

	// Each remote user gets its own throttled updater instead of one shared
	// across the whole roster: a single shared throttle's trailing call can
	// fire after its subject already left, resurrecting their cursor
	// updateRoomPresence cancels and drops the entry for any
	// user who leaves the roster.
	const getThrottledCursorUpdater = useCallback(
		(userId: string) => {
			const existing = cursorThrottleMapRef.current.get(userId)
			if (existing) {
				return existing
			}

			const throttled = throttle(doUpdateCursor, CURSOR_UPDATE_DELAY, { leading: false, trailing: true })
			cursorThrottleMapRef.current.set(userId, throttled)
			return throttled
		},
		[doUpdateCursor],
	)

	const updateCursorState = useCallback(
		(payload: CollaboratorPayload) => {
			if (!payload.user?.id || !payload.pointer) {
				logger.warn('[Collaboration] Invalid cursor payload:', payload)
				return
			}

			getThrottledCursorUpdater(payload.user.id)(payload)
		},
		[getThrottledCursorUpdater],
	)

	const clearExcalidrawCollaborators = useCallback(() => {
		cursorThrottleMapRef.current.forEach(throttledFn => throttledFn.cancel())
		cursorThrottleMapRef.current.clear()

		if (excalidrawAPI) {
			excalidrawAPI.updateScene({ collaborators: new Map() })
		}
	}, [excalidrawAPI])

	const clearJoinRetry = useCallback(() => {
		if (joinRetryTimeoutRef.current) {
			clearTimeout(joinRetryTimeoutRef.current)
			joinRetryTimeoutRef.current = null
		}
	}, [])

	// So a stale timer from an earlier wrong-replica rejection never revives
	// a socket the component already dropped for another reason (a normal
	// reconnect, an unmount, a real disconnect).
	const clearWrongReplicaRetry = useCallback(() => {
		if (wrongReplicaRetryTimeoutRef.current) {
			clearTimeout(wrongReplicaRetryTimeoutRef.current)
			wrongReplicaRetryTimeoutRef.current = null
		}
	}, [])

	// Emits join-room and arms a re-join safety net: the relay confirms a
	// join with room-user-change (cleared by that handler below), so if none
	// arrives within JOIN_CONFIRM_TIMEOUT the join is presumed lost and
	// retried, bounded by MAX_JOIN_RETRIES.
	const attemptJoinRoom = useCallback(
		function attemptJoinRoom(socket: CollaborationSocket, roomId: string) {
			joinedRoomRef.current = roomId
			socket.emit('join-room', roomId)

			clearJoinRetry()
			joinRetryTimeoutRef.current = setTimeout(() => {
				if (!shouldRetryJoin(joinRetryCountRef.current, MAX_JOIN_RETRIES)) {
					logger.error(`[Collaboration] Giving up joining room ${roomId} after ${MAX_JOIN_RETRIES} attempts`)
					return
				}

				joinRetryCountRef.current += 1
				joinedRoomRef.current = null
				logger.warn(`[Collaboration] No room-user-change after joining ${roomId}, retrying (${joinRetryCountRef.current}/${MAX_JOIN_RETRIES})`)

				const latestSocket = useCollaborationStore.getState().socket
				if (latestSocket?.connected) {
					attemptJoinRoom(latestSocket, roomId)
				}
			}, JOIN_CONFIRM_TIMEOUT)
		},
		[clearJoinRetry],
	)

	// Debounced so multiple rapid init-room/reconnect events collapse into
	// one join attempt.
	const debouncedJoinRoom = useMemo(() =>
		debounce((socket: CollaborationSocket, roomId: string) => {
			logger.debug(`[Collaboration] Debounced join room ${roomId}`)
			joinRetryCountRef.current = 0
			attemptJoinRoom(socket, roomId)
		}, 300, { leading: true, trailing: false }),
	[attemptJoinRoom])

	const handleInitRoom = useCallback(() => {
		const currentSocket = useCollaborationStore.getState().socket

		if (!fileId || !currentSocket || !currentSocket.connected) {
			logger.warn('[Collaboration] Cannot join room:', {
				hasFileId: !!fileId,
				hasSocket: !!currentSocket,
				connected: currentSocket?.connected,
			})
			return
		}

		if (!shouldJoinRoom(joinedRoomRef.current, fileId)) {
			logger.debug(`[Collaboration] Already joined room ${fileId}, skipping`)
			return
		}

		logger.debug(`[Collaboration] Joining room ${fileId}`)
		// joinedRoomRef is set inside attemptJoinRoom, after the emit: setting
		// it here first, before the debounce actually fires, could mark a
		// room "joined" that a dropped debounce call never
		// really sent a join-room for.
		debouncedJoinRoom(currentSocket, fileId)
	}, [fileId, debouncedJoinRoom])

	const handleClientBroadcast = useCallback(
		async (data: ArrayBuffer) => {
			const decoded = decodeClientBroadcast(data)
			if (!decoded) {
				logger.warn('[Collaboration] Invalid or unrecognized broadcast payload')
				return
			}

			switch (decoded.type) {
			case 'SCENE_INIT':
			case 'SCENE_UPDATE': {
				const elementsString = JSON.stringify(decoded.payload.elements)
				if (isDuplicateScenePayload(sceneDedupRef.current, elementsString)) {
					logger.debug('[Collaboration] Received identical scene payload, skipping update')
					break
				}
				// sceneDedupRef is only marked once this payload is actually
				// applied (reconcileAndApplyRemoteElements), not here at decode
				// time.
				queueSceneUpdate(decoded.payload.elements)
				break
			}
			case 'MOUSE_LOCATION':
				updateCursorState(decoded.payload)
				break
			case 'IMAGE_ADD':
				queueImageUpdate(decoded.payload.file)
				break
			case 'IMAGE_REQUEST': {
				// The relay drops a server-broadcast from a read-only session
				// anyway; skip the encode-and-emit entirely instead of doing it
				// for nothing.
				if (!excalidrawAPI || !fileId || useSessionStore.getState().isReadOnly) break
				const file = excalidrawAPI.getFiles()[decoded.payload.fileId]
				if (!file?.dataURL) break

				const currentSocket = useCollaborationStore.getState().socket
				if (!currentSocket?.connected) break

				const fileData = { type: 'IMAGE_ADD', payload: { file } }
				const fileBuffer = new TextEncoder().encode(JSON.stringify(fileData))
				currentSocket.emit('server-broadcast', fileId, fileBuffer, [])
				break
			}
			}
		},
		[queueSceneUpdate, updateCursorState, queueImageUpdate, excalidrawAPI, fileId],
	)

	const handleSyncDesignate = useCallback((data: { isSyncer: boolean }) => {
		logger.debug(`[Collaboration] Sync designation received: ${data.isSyncer}`)
		setDedicatedSyncer(data.isSyncer)
	}, [setDedicatedSyncer])

	// Handle user joined event - broadcast the current full scene and images if we're the syncer
	const handleUserJoined = useCallback((data: { userId: string, userName: string, socketId: string, isSyncer: boolean }) => {
		const { isDedicatedSyncer } = useCollaborationStore.getState()
		// The relay drops a server-broadcast from a read-only session; a
		// read-only client is never the dedicated syncer in practice (syncer
		// election also excludes it), but guard here too so this never does
		// the encode-and-emit work for nothing.
		if (isDedicatedSyncer && excalidrawAPI && !useSessionStore.getState().isReadOnly) {
			logger.debug(`[Collaboration] Broadcasting full scene to new user: ${data.userName}`)

			const elements = excalidrawAPI.getSceneElementsIncludingDeleted()
			const files = excalidrawAPI.getFiles()
			const socket = useCollaborationStore.getState().socket

			if (!socket || !socket.connected || !fileId) return

			const sceneData = { type: 'SCENE_INIT', payload: { elements } }
			const sceneBuffer = new TextEncoder().encode(JSON.stringify(sceneData))
			socket.emit('server-broadcast', fileId, sceneBuffer, [])

			logger.debug('[Collaboration] Re-broadcasted full scene for room convergence', {
				targetUser: data.userName,
				elementsCount: elements.length,
			})

			Object.entries(files).forEach(([, file]) => {
				if (file && file.dataURL) {
					const fileData = { type: 'IMAGE_ADD', payload: { file } }
					const fileBuffer = new TextEncoder().encode(JSON.stringify(fileData))
					socket.emit('server-broadcast', fileId, fileBuffer, [])
				}
			})
		}
	}, [excalidrawAPI, fileId])

	const setupSocketEventHandlers = useCallback(
		(socketInstance: CollaborationSocket) => {
			socketInstance.removeAllListeners()
			// removeAllListeners() only clears Socket-level listeners; the four
			// reconnection events below live on the Manager (socketInstance.io)
			// and need their own off() or the reuse path (connectSocket handing
			// back an existing socketInstanceRef) piles up duplicate Manager
			// listeners on every reconnect.
			socketInstance.io.off('reconnect')
			socketInstance.io.off('reconnect_attempt')
			socketInstance.io.off('reconnect_error')
			socketInstance.io.off('reconnect_failed')

			socketInstance.on('connect_error', (error: Error) => {
				logger.error('[Collaboration] Connection Error:', error.message)
				const errorClass = classifyConnectError(error.message)

				if (errorClass === 'wrong-replica') {
					// internal/relay/relay.go's wrongReplicaMessage rejects the
					// handshake in namespace middleware when the session's file
					// currently belongs to another replica. socket.io-client v4
					// treats any middleware rejection as fatal: Socket.destroy()
					// tells the Manager to _close(), which sets skipReconnect and
					// permanently disables the built-in reconnect loop (socket.js
					// ~614-628, manager.js's _close()) — so there is nothing to wait
					// for, and the client must rebuild the socket itself.
					logger.warn('[Collaboration] Wrong-replica rejection, scheduling a fresh connection attempt')
					setStatus('reconnecting')
					scheduleWrongReplicaRetryRef.current()
					return
				}

				if (errorClass === 'auth-failure') {
					// The session JWT is minted once, at launch, with no
					// refresh path, so an auth rejection can never resolve itself —
					// the first failure is already terminal. socket.io-
					// client v4 also destroys the socket on a middleware auth
					// rejection before this fires, so there is no automatic retry to
					// wait for either.
					markTerminalAuthFailure('jwt_secret_mismatch', 'WebSocket authentication failed - possible JWT secret mismatch')
					logger.warn('[Collaboration] Persistent authentication failure detected (no refresh path); stopping reconnection attempts')
					socketInstance.disconnect()
					setStatus('offline')
					return
				}
				setStatus('offline')
			})

			socketInstance.on('connect', () => {
				logger.debug('[Collaboration] Socket connect event fired - setting status to online')
				// A normal connect means the wrong-replica retry (if one was
				// pending) already did its job or is no longer needed; a stale
				// timer left running here could otherwise fire later and rebuild
				// a socket this component no longer wants.
				clearWrongReplicaRetry()
				setStatus('online')
				setIsInRoom(false)

				const { authError } = useCollaborationStore.getState()
				if (authError.type !== 'jwt_secret_mismatch') {
					clearAuthError()
				}

				joinedRoomRef.current = null
			})

			socketInstance.on('disconnect', (reason) => {
				logger.warn(`[Collaboration] Socket disconnect event fired: ${reason}`)
				clearExcalidrawCollaborators()
				setIsInRoom(false)

				if (reason === 'io client disconnect') {
					setStatus('offline')
				} else {
					setStatus('reconnecting')
				}

				joinedRoomRef.current = null
			})

			// Reconnection events (unlike connect/disconnect/connect_error) fire
			// on the socket's Manager, not the socket itself
			// (socket.io-client's SocketReservedEvents only reserves
			// connect/connect_error/disconnect on the Socket type).
			socketInstance.io.on('reconnect', () => {
				clearWrongReplicaRetry()
				setStatus('online')
				setIsInRoom(false)

				const { authError } = useCollaborationStore.getState()
				if (authError.type !== 'jwt_secret_mismatch') {
					clearAuthError()
				}

				joinedRoomRef.current = null
			})

			socketInstance.io.on('reconnect_attempt', () => {
				setStatus('reconnecting')
			})

			socketInstance.io.on('reconnect_error', (error: Error) => {
				logger.error('[Collaboration] Reconnection error:', error)
			})

			socketInstance.io.on('reconnect_failed', () => {
				logger.error('[Collaboration] Reconnection failed - giving up')
				setStatus('offline')
			})

			socketInstance.on('init-room', () => {
				joinedRoomRef.current = null

				const currentStatus = useCollaborationStore.getState().status
				if ((currentStatus === 'connecting' || currentStatus === 'offline') && socketInstance.connected) {
					setStatus('online')

					const { authError } = useCollaborationStore.getState()
					if (authError.type !== 'jwt_secret_mismatch') {
						clearAuthError()
					}
				}

				handleInitRoom()
			})

			socketInstance.on('room-user-change', (entries) => {
				// A roster update from the relay is the join confirmation the
				// re-join safety net (attemptJoinRoom) waits for.
				clearJoinRetry()
				joinRetryCountRef.current = 0

				setIsInRoom(true)
				updateRoomPresence(entries)
			})

			socketInstance.on('client-broadcast', handleClientBroadcast)
			socketInstance.on('sync-designate', handleSyncDesignate)

			socketInstance.on('user-joined', (data) => {
				handleUserJoined(data)
			})

			socketInstance.on('error', (message) => {
				// The relay emits this on a claim mismatch or an oversized
				// broadcast; there is no client action to take beyond
				// surfacing it.
				logger.error('[Collaboration] Relay error:', message)
			})

			// A server-pushed conflict state change. The initial GET
			// /api/board/conflict and its poll fallback (App.tsx) answer the
			// same shape; this is the live, low-latency path while the socket
			// stays connected.
			socketInstance.on('conflict-state', (data) => {
				logger.debug('[Collaboration] Conflict state changed:', data)
				setConflict(data.inConflict)
				setSaveStalled(data.saveStalled)
			})

			// The room's retained scene is gone
			// server-side once another client resolved a conflict on the
			// reload branch, so this client must reload too, the same way
			// ConflictBanner's conflict-resolution Reload button does (a
			// full page reload, not a fetch-and-swap: see that component's
			// comment for why).
			socketInstance.on('reload-required', async () => {
				logger.debug('[Collaboration] Server requested a reload')
				// Set before reload(), same reason as ConflictBanner's
				// handleReload: see useCollaborationStore's `reloading` field.
				useCollaborationStore.getState().setReloading(true)
				// This client did not choose to discard its own edits, but the
				// room's retained scene is already gone server-side (see the
				// comment on this listener's registration above), so any local
				// pending changes must not come back through the IndexedDB
				// reconcile on the next load either (see db.clearPendingLocalChanges).
				// Never let a storage error block the reload itself.
				try {
					const currentFileId = useWhiteboardConfigStore.getState().fileId
					if (currentFileId) {
						await db.clearPendingLocalChanges(currentFileId)
					}
				} catch (error) {
					logger.error('[Collaboration] Failed to clear pending local changes before reload:', error)
				} finally {
					window.location.reload()
				}
			})

			return socketInstance
		},
		[
			setStatus, handleInitRoom, updateRoomPresence, handleClientBroadcast, handleSyncDesignate,
			clearExcalidrawCollaborators, handleUserJoined, markTerminalAuthFailure, clearAuthError, setIsInRoom,
			clearJoinRetry, setConflict, setSaveStalled, clearWrongReplicaRetry,
		],
	)

	const socketInstanceRef = useRef<CollaborationSocket | null>(null)

	const connectSocket = useCallback(() => {
		const { socket: currentSocket, status: currentStatus } = useCollaborationStore.getState()

		if (!fileId) {
			logger.warn('[Collaboration] Cannot connect: invalid fileId.')
			setStatus('offline')
			return
		}

		if (currentStatus === 'online' || currentStatus === 'connecting') {
			return
		}

		const { authError } = useCollaborationStore.getState()
		if (authError.isPersistent && authError.type === 'jwt_secret_mismatch') {
			logger.warn('[Collaboration] Skipping connection attempt due to persistent JWT secret mismatch')
			setStatus('offline')
			return
		}

		joinedRoomRef.current = null

		if (currentSocket) {
			currentSocket.disconnect()
			setSocket(null)
		}

		try {
			setStatus('connecting')

			const token = useSessionStore.getState().getToken()
			if (!token) throw new Error('Session token missing.')

			const path = socketPath.endsWith('/socket.io') ? socketPath : `${socketPath}/socket.io`

			if (socketInstanceRef.current) {
				socketInstanceRef.current.auth = { token }
				setupSocketEventHandlers(socketInstanceRef.current)
				setSocket(socketInstanceRef.current)

				if (!socketInstanceRef.current.connected) {
					socketInstanceRef.current.connect()
				}
				return
			}

			const newSocket = io('/', {
				path,
				// query is a Manager-level option: engine.io-client appends it to
				// every polling and websocket handshake URL it opens, not just the
				// namespace CONNECT payload (see the router note in
				// utils/roomParam.ts for why the URL is what a router reads).
				query: { room: fileId },
				auth: { token },
				transports: ['websocket'],
				reconnection: true,
				reconnectionDelay: 1000,
				reconnectionDelayMax: 10000,
				reconnectionAttempts: Infinity,
				perMessageDeflate: {
					threshold: 1024, // Only compress messages larger than 1KB
				},
			}) as unknown as CollaborationSocket

			socketInstanceRef.current = newSocket

			setupSocketEventHandlers(newSocket)
			setSocket(newSocket)
			newSocket.connect()
		} catch (error) {
			logger.error('[Collaboration] Connection initiation failed:', error)
			setSocket(null)
			socketInstanceRef.current = null
			setStatus('offline')
		}
	}, [setStatus, setSocket, setupSocketEventHandlers, fileId, socketPath])

	// setupSocketEventHandlers is otherwise only called from inside
	// connectSocket, which early-returns once status is 'online' or
	// 'connecting' — so if excalidrawAPI arrives after that point, the
	// handlers stay bound to the stale closure captured before it existed.
	// The local convergence test does not exercise this path because, on a
	// normal mount, Excalidraw's excalidrawAPI callback runs in a child
	// effect, which React commits before this hook's own connectSocket
	// effect (a parent effect) ever runs — so the very first
	// setupSocketEventHandlers call already closes over a live
	// excalidrawAPI, and the race this effect guards against never fires
	// in that test. The pending-queue drain effect above only catches a
	// delayed excalidrawAPI once; it does not rebind handlers, so a
	// broadcast arriving after that one drain would still hit a stale
	// closure without this effect.
	useEffect(() => {
		if (socketInstanceRef.current) {
			setupSocketEventHandlers(socketInstanceRef.current)
		}
	}, [setupSocketEventHandlers, socketId])

	const disconnectSocket = useCallback(() => {
		const currentSocket = useCollaborationStore.getState().socket
		const socketToClose = socketInstanceRef.current ?? currentSocket

		if (socketToClose) {
			socketToClose.removeAllListeners()
			socketToClose.io.off('reconnect')
			socketToClose.io.off('reconnect_attempt')
			socketToClose.io.off('reconnect_error')
			socketToClose.io.off('reconnect_failed')
			socketToClose.disconnect()

			setSocket(null)
			setStatus('offline')
			clearExcalidrawCollaborators()
			setIsInRoom(false)

			joinedRoomRef.current = null
		}

		socketInstanceRef.current = null
	}, [setSocket, setStatus, clearExcalidrawCollaborators, setIsInRoom])

	// Rebuilds the socket after a wrong-replica connect_error: tears the
	// destroyed socket down through the same disconnectSocket/connectSocket
	// pair the fileId effect below uses, rather than a parallel reconnect
	// path, since that pair already does the full app-level teardown
	// (listeners, store state, joinedRoomRef) and a from-scratch io() call.
	const scheduleWrongReplicaRetry = useCallback(() => {
		clearWrongReplicaRetry()
		disconnectSocket()
		wrongReplicaRetryTimeoutRef.current = setTimeout(() => {
			wrongReplicaRetryTimeoutRef.current = null
			logger.warn('[Collaboration] Retrying connection after a wrong-replica rejection')
			connectSocket()
		}, WRONG_REPLICA_RETRY_DELAY)
	}, [clearWrongReplicaRetry, disconnectSocket, connectSocket])

	useEffect(() => {
		scheduleWrongReplicaRetryRef.current = scheduleWrongReplicaRetry
	}, [scheduleWrongReplicaRetry])

	// Connect/Disconnect based on fileId
	useEffect(() => {
		if (fileId) {
			connectSocket()
		} else {
			disconnectSocket()
			setIsInRoom(false)
		}

		return () => {
			debouncedJoinRoom.cancel()
			clearJoinRetry()
		}
	}, [fileId, connectSocket, disconnectSocket, debouncedJoinRoom, setIsInRoom, clearJoinRetry])

	// disconnectSocket's identity changes mid-session: it depends on
	// clearExcalidrawCollaborators, which depends on excalidrawAPI. An
	// unmount effect that lists it as a dependency would tear the live
	// socket down on every such change, not only on a real unmount. These
	// refs hold the latest version of everything the cleanup below
	// needs, so that effect can take an empty dependency array instead, and
	// fire only once, on the real unmount.
	const disconnectSocketRef = useRef(disconnectSocket)
	const resetStoreRef = useRef(resetStore)
	const debouncedJoinRoomRef = useRef(debouncedJoinRoom)
	const clearJoinRetryRef = useRef(clearJoinRetry)
	const clearWrongReplicaRetryRef = useRef(clearWrongReplicaRetry)

	useEffect(() => {
		disconnectSocketRef.current = disconnectSocket
		resetStoreRef.current = resetStore
		debouncedJoinRoomRef.current = debouncedJoinRoom
		clearJoinRetryRef.current = clearJoinRetry
		clearWrongReplicaRetryRef.current = clearWrongReplicaRetry
	})

	// Cleanup on unmount only: an empty dependency array means
	// this effect's cleanup fires exactly once, on real unmount.
	useEffect(() => {
		return () => {
			debouncedJoinRoomRef.current.cancel()
			clearJoinRetryRef.current()
			clearWrongReplicaRetryRef.current()

			disconnectSocketRef.current()
			resetStoreRef.current()
		}
	}, [])

	return {
		connect: connectSocket,
		disconnect: disconnectSocket,
	}
}
