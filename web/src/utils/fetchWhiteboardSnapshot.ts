/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// The Go board API (internal/boardapi) answers GET /api/board with the raw
// scene JSON, unenveloped: either the last scene a client PUT, or, on a
// cache miss, the bytes of the stored .excalidraw file straight from
// the host's WOPI GetFile (a full export, which carries top-level
// `type`/`version`/`source` fields this function ignores). Neither shape
// carries scrollToContent; callers decide that themselves
// (useBoardDataManager scrolls to content only when the scene is non-empty).

import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState, BinaryFiles } from '@excalidraw/excalidraw/types'
import logger from './logger'
import { withRoomParam } from './roomParam'

export interface WhiteboardSnapshot {
	elements: readonly ExcalidrawElement[]
	files: BinaryFiles
	appState?: Partial<AppState>
}

/**
 * Thrown when the board API request itself did not work: a network error, a
 * non-404 error status, or a response body that is not the expected shape.
 * Callers must not treat this the same as a confirmed-empty board (a 404) —
 * see useBoardDataManager's loadBoard for why the distinction matters.
 */
export class WhiteboardSnapshotFetchError extends Error {

	constructor(message: string, options?: { cause?: unknown }) {
		super(message, options)
		this.name = 'WhiteboardSnapshotFetchError'
	}

}

/**
 * Fetches the current board scene from the Go server's board API.
 * @param apiBase the AppConfig.apiBase prefix (e.g. `/api`)
 * @param token the session JWT, sent as a bearer token
 * @param fileId the AppConfig.fileId, sent as the router's room query
 * parameter (see utils/roomParam.ts)
 * @returns the scene, or null when the board has no server data yet (a 404
 * — the board is genuinely empty, not that the fetch failed)
 * @throws WhiteboardSnapshotFetchError when the request fails outright: a
 * network error, a non-404 error status, or a malformed response body
 */
export async function fetchWhiteboardSnapshot(
	apiBase: string,
	token: string,
	fileId: string,
): Promise<WhiteboardSnapshot | null> {
	let response: Response
	try {
		response = await fetch(withRoomParam(`${apiBase}/board`, fileId), {
			method: 'GET',
			headers: {
				Authorization: `Bearer ${token}`,
			},
		})
	} catch (error) {
		logger.error('[fetchWhiteboardSnapshot] Error fetching board snapshot:', error)
		throw new WhiteboardSnapshotFetchError('Network error while fetching the board snapshot', { cause: error })
	}

	if (!response.ok) {
		if (response.status === 404) {
			return null
		}
		logger.error(`[fetchWhiteboardSnapshot] Server responded with status: ${response.status}`)
		throw new WhiteboardSnapshotFetchError(`Server responded with status ${response.status}`)
	}

	let body: string
	try {
		body = await response.text()
	} catch (error) {
		logger.error('[fetchWhiteboardSnapshot] Error reading board snapshot body:', error)
		throw new WhiteboardSnapshotFetchError('Network error while reading the board snapshot body', { cause: error })
	}

	// A 0-byte 200 is a confirmed-empty board, not a malformed body: a
	// WOPI host serves a new board as an empty file (the empty-file
	// PutFile rule), so treat it like the 404 case above.
	if (body.trim() === '') {
		return null
	}

	let data: unknown
	try {
		data = JSON.parse(body)
	} catch (error) {
		logger.error('[fetchWhiteboardSnapshot] Failed to parse response body as JSON:', error)
		throw new WhiteboardSnapshotFetchError('Malformed board snapshot response body', { cause: error })
	}

	if (!data || typeof data !== 'object' || !Array.isArray((data as { elements?: unknown }).elements)) {
		logger.error('[fetchWhiteboardSnapshot] Response body has no elements array:', data)
		throw new WhiteboardSnapshotFetchError('Board snapshot response has no elements array')
	}

	const snapshot = data as { elements: ExcalidrawElement[]; files?: BinaryFiles; appState?: Partial<AppState> }

	return {
		elements: snapshot.elements,
		files: snapshot.files || {},
		appState: snapshot.appState,
	}
}
