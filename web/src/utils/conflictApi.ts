// SPDX-License-Identifier: AGPL-3.0-or-later

// Talks to the two conflict endpoints internal/boardapi exposes (GET
// /api/board/conflict, POST /api/board/conflict/resolve). Mirrors
// fetchWhiteboardSnapshot.ts's shape: a best-effort fetch a caller can poll
// or call once on load, and a small action for the resolve POST.

import logger from './logger'
import { withRoomParam } from './roomParam'

/** The GET /api/board/conflict and conflict-state socket wire shape. */
export interface ConflictState {
	inConflict: boolean
	saveStalled: boolean
}

/**
 * Fetches the caller's room's current conflict state.
 * @param apiBase the AppConfig.apiBase prefix (e.g. `/api`)
 * @param token the session JWT, sent as a bearer token
 * @param fileId the AppConfig.fileId, sent as the router's room query
 * parameter (see utils/roomParam.ts)
 * @returns the room's conflict state, or null when the request itself
 * failed (network error, non-2xx status, malformed body) — callers treat
 * that as "unknown", not as "no conflict", so a transient failure never
 * hides a real conflict banner nor spuriously shows one.
 */
export async function fetchConflictState(apiBase: string, token: string, fileId: string): Promise<ConflictState | null> {
	let response: Response
	try {
		response = await fetch(withRoomParam(`${apiBase}/board/conflict`, fileId), {
			method: 'GET',
			headers: { Authorization: `Bearer ${token}` },
		})
	} catch (error) {
		logger.warn('[conflictApi] Network error while fetching conflict state:', error)
		return null
	}

	if (!response.ok) {
		logger.warn(`[conflictApi] GET /board/conflict responded with status ${response.status}`)
		return null
	}

	try {
		const data: unknown = await response.json()
		if (
			!data || typeof data !== 'object'
			|| typeof (data as { inConflict?: unknown }).inConflict !== 'boolean'
			|| typeof (data as { saveStalled?: unknown }).saveStalled !== 'boolean'
		) {
			logger.warn('[conflictApi] Malformed conflict state response body:', data)
			return null
		}
		return data as ConflictState
	} catch (error) {
		logger.warn('[conflictApi] Failed to parse conflict state response body:', error)
		return null
	}
}

/**
 * Resolves the caller's room's conflict: overwrite=true forces the
 * client's retained scene to the WOPI host; overwrite=false drops it so
 * the next load proxies fresh host content.
 * @param fileId the AppConfig.fileId, sent as the router's room query
 * parameter (see utils/roomParam.ts)
 * @returns whether the POST succeeded
 */
export async function resolveConflict(apiBase: string, token: string, fileId: string, overwrite: boolean): Promise<boolean> {
	try {
		const response = await fetch(withRoomParam(`${apiBase}/board/conflict/resolve`, fileId), {
			method: 'POST',
			headers: {
				Authorization: `Bearer ${token}`,
				'Content-Type': 'application/json',
			},
			body: JSON.stringify({ overwrite }),
		})
		if (!response.ok) {
			logger.error(`[conflictApi] POST /board/conflict/resolve responded with status ${response.status}`)
			return false
		}
		return true
	} catch (error) {
		logger.error('[conflictApi] Network error while resolving conflict:', error)
		return false
	}
}
