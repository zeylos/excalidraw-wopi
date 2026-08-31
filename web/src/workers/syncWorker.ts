/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// SYNC_TO_SERVER PUTs `{ elements, files }` straight to the room's board
// resource; the URL and the bearer token arrive in the message. The
// 409-treated-as-skipped behavior and the lastSyncedHash write-back on
// success are kept.
// handleSyncToLocal/handleSyncToServer/handleMessage return the outbound
// message instead of calling postMessage directly, so they are plain,
// testable functions; only the addEventListener glue at the bottom performs
// the actual postMessage side effect. Two type casts bridge a mismatch
// between two other files: WorkerInboundMessage's `elements` is
// `readonly ExcalidrawElement[]` but db.put() takes a mutable array, and its
// SYNC_TO_LOCAL `appState` is `Partial<AppState>` but db.put() takes a full
// `AppState`.

import { db } from '../database/db'
import { computeElementVersionHash } from '../utils/syncSceneData'
import type { WorkerInboundMessage, WorkerOutboundMessage } from '../types/protocol'
import type { AppState } from '@excalidraw/excalidraw/types'

let performance: Performance
try {
	performance = self.performance
} catch {
	performance = {
		now: () => Date.now(),
	} as Performance
}

const error = (message: string, ...args: unknown[]) => {
	try {
		globalThis.console.error(`[SyncWorker] ${message}`, ...args)
	} catch {
		// Ignore logging errors inside worker
	}
}

type SyncToLocalMessage = Extract<WorkerInboundMessage, { type: 'SYNC_TO_LOCAL' }>
type SyncToServerMessage = Extract<WorkerInboundMessage, { type: 'SYNC_TO_SERVER' }>

export const handleSyncToLocal = async (data: SyncToLocalMessage): Promise<WorkerOutboundMessage> => {
	const { fileId, elements, files, appState } = data

	if (!fileId) {
		error('Missing fileId for local sync')
		return { type: 'LOCAL_SYNC_ERROR', error: 'Missing fileId for local sync' }
	}

	const startTime = performance.now()

	try {
		const filteredAppState = appState ? { ...appState } : appState

		if (filteredAppState && filteredAppState.collaborators) {
			delete filteredAppState.collaborators
		}

		await db.put(fileId, [...elements], files || {}, filteredAppState as AppState | undefined, {
			hasPendingLocalChanges: true,
		})

		const duration = performance.now() - startTime

		return {
			type: 'LOCAL_SYNC_COMPLETE',
			duration,
			elementsCount: elements.length,
		}
	} catch (e) {
		error('Error syncing to local storage:', e)
		return {
			type: 'LOCAL_SYNC_ERROR',
			error: e instanceof Error ? e.message : String(e),
		}
	}
}

export const handleSyncToServer = async (data: SyncToServerMessage): Promise<WorkerOutboundMessage> => {
	const { fileId, url, jwt, elements, files } = data

	if (!fileId || !url || !jwt) {
		error('Missing required data for server sync', { fileId, url: !!url, jwt: !!jwt })
		return { type: 'SERVER_SYNC_ERROR', error: 'Missing required data for server sync' }
	}

	const startTime = performance.now()

	try {
		const response = await globalThis.fetch(url, {
			method: 'PUT',
			headers: {
				'Content-Type': 'application/json',
				Authorization: `Bearer ${jwt}`,
			},
			body: JSON.stringify({ elements, files: files || {} }),
		})

		if (response.status === 409) {
			return {
				type: 'SERVER_SYNC_COMPLETE',
				success: true,
				skipped: true,
				duration: 0,
				elementsCount: elements.length,
			}
		}

		if (!response.ok) {
			let errorMessage = `Server responded with status: ${response.status}`
			try {
				const responseText = await response.text()
				errorMessage += ` - ${responseText}`
			} catch {
				// Ignore parse errors
			}
			throw new Error(errorMessage)
		}

		let responseData: unknown
		try {
			responseData = await response.json()
		} catch {
			// Non-JSON response still counts as success
		}

		try {
			const existing = await db.get(fileId)
			await db.put(
				fileId,
				[...elements],
				files || existing?.files || {},
				existing?.appState,
				{
					hasPendingLocalChanges: false,
					lastSyncedHash: computeElementVersionHash(elements),
				},
			)
		} catch (dbError) {
			error('Error updating local metadata after server sync:', dbError)
		}

		const duration = performance.now() - startTime

		return {
			type: 'SERVER_SYNC_COMPLETE',
			success: true,
			duration,
			elementsCount: elements.length,
			response: responseData,
		}
	} catch (e) {
		error('Error syncing to server:', e)
		return {
			type: 'SERVER_SYNC_ERROR',
			error: e instanceof Error ? e.message : String(e),
		}
	}
}

export const handleMessage = async (message: WorkerInboundMessage): Promise<WorkerOutboundMessage> => {
	try {
		switch (message.type) {
		case 'INIT':
			return { type: 'INIT_COMPLETE' }
		case 'SYNC_TO_LOCAL':
			return await handleSyncToLocal(message)
		case 'SYNC_TO_SERVER':
			return await handleSyncToServer(message)
		}
	} catch (e) {
		error(`Error handling message ${message.type}:`, e)
		const errorMessage = e instanceof Error ? e.message : String(e)

		if (message.type === 'SYNC_TO_LOCAL') {
			return { type: 'LOCAL_SYNC_ERROR', error: errorMessage }
		} else if (message.type === 'SYNC_TO_SERVER') {
			return { type: 'SERVER_SYNC_ERROR', error: errorMessage }
		}
		return { type: 'INIT_ERROR', error: errorMessage }
	}
}

const ctx: Worker = self as unknown as Worker

const sendMessage = (message: WorkerOutboundMessage) => {
	try {
		ctx.postMessage(message)
	} catch (e) {
		error(`Failed to send message: ${message.type}`, e)
	}
}

// Serializes every inbound message through one promise chain. Without
// this, two messages posted back-to-back (e.g. a SYNC_TO_LOCAL
// racing an in-flight SYNC_TO_SERVER's db.put) run handleMessage at the same
// time. Whichever fetch or db write settles last then wins, regardless of
// which message actually arrived last. The .catch() folds a rejection back
// into a resolved chain, so one failed message never blocks every message
// after it.
let messageQueue: Promise<void> = Promise.resolve()

ctx.addEventListener('message', (event: MessageEvent<WorkerInboundMessage>) => {
	messageQueue = messageQueue
		.then(() => handleMessage(event.data))
		.then(sendMessage)
		.catch((e) => error('Unhandled error in message handler', e))
})
