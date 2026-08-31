// SPDX-License-Identifier: AGPL-3.0-or-later

// Kept separate from useCollaboration.ts's handleClientBroadcast so the
// decode-and-validate step is testable without a socket, a store, or the
// Excalidraw API. The hook owns the switch that turns a decoded message
// into an action; this only decides whether the bytes are safe to act on.

import type { BinaryFileData } from '@excalidraw/excalidraw/types'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { CollaboratorPayload, SceneBroadcastMessage } from '../types/collaboration'

/**
 * Decodes a client-broadcast ArrayBuffer as UTF-8 JSON and validates it
 * against the SceneBroadcastMessage union's shape: the relay forwards these
 * bytes untouched, so nothing before this function has validated them.
 * Returns null for malformed JSON, a missing/unknown
 * `type`, or a payload missing the fields that type requires.
 */
export function decodeClientBroadcast(data: ArrayBuffer): SceneBroadcastMessage | null {
	let decoded: unknown
	try {
		decoded = JSON.parse(new TextDecoder().decode(data))
	} catch {
		return null
	}

	if (!decoded || typeof decoded !== 'object' || !('type' in decoded)) {
		return null
	}

	const { type, payload } = decoded as { type: unknown; payload: unknown }
	if (typeof type !== 'string' || !payload || typeof payload !== 'object') {
		return null
	}
	const p = payload as Record<string, unknown>

	switch (type) {
	case 'SCENE_INIT':
	case 'SCENE_UPDATE':
		if (!Array.isArray(p.elements)) return null
		return { type, payload: { elements: p.elements as ExcalidrawElement[] } }

	case 'MOUSE_LOCATION':
		if (!isCollaboratorPayload(p)) return null
		return { type, payload: p as unknown as CollaboratorPayload }

	case 'IMAGE_ADD': {
		if (!p.file || typeof p.file !== 'object') return null
		const file = p.file as Record<string, unknown>
		// A dataURL that is not actually a data: URI would make every
		// browser in the room fetch it as an <img> src, leaking each
		// viewer's IP/user-agent/referer to whatever host a malicious peer
		// points it at, and could taint the canvas with cross-origin
		// content.
		if (typeof file.dataURL !== 'string' || !file.dataURL.startsWith('data:image/')) return null
		return { type, payload: { file: p.file as BinaryFileData } }
	}

	case 'IMAGE_REQUEST':
		if (typeof p.fileId !== 'string') return null
		return { type, payload: { fileId: p.fileId } }

	default:
		return null
	}
}

function isCollaboratorPayload(p: Record<string, unknown>): boolean {
	// user.id/user.name are trusted only as far as their shape: the relay
	// stamps this field with the sender's real identity, but a malformed or
	// foreign-shaped value here must not reach the collaborators map.
	const user = p.user
	return Boolean(
		user && typeof user === 'object'
		&& typeof (user as Record<string, unknown>).id === 'string'
		&& typeof (user as Record<string, unknown>).name === 'string'
		&& p.pointer && typeof p.pointer === 'object'
		&& (p.button === 'down' || p.button === 'up'),
	)
}
