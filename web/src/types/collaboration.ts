/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// This project has no recording, presentation, or voting feature, so their
// event typings (RecordingStoppedPayload, RecordingAvailabilityPayload,
// ViewportRequestPayload, and the recording/presentation/voting entries on
// ServerToClientEvents/ClientToServerEvents) do not exist here. The relay
// supports only init-room, join-room, sync-designate, room-user-change,
// user-joined, client-broadcast, server-broadcast,
// server-volatile-broadcast, image-get, error, conflict-state, and
// reload-required. There is no SCENE_RESTORE: the live reload mechanism is
// the `reload-required` socket event. There is no VIEWPORT_UPDATE: this
// project has no viewport-following feature, so nothing emits it. The
// scene-broadcast payload union (SCENE_INIT/SCENE_UPDATE/MOUSE_LOCATION/
// IMAGE_ADD/IMAGE_REQUEST) is defined here, in one place, since both
// useCollaboration.ts and useSync.ts need it.
//
// `room-user-change` and `CollaboratorPayload` split roster identity from
// live pointer state. The Go relay (internal/relay/relay.go's
// presenceEntry, wirePresence) keeps the two separate: a
// `room-user-change` entry carries only roster identity (socketId, user,
// userId, socketIds), never a live pointer. Live pointer state travels only
// through a MOUSE_LOCATION client-broadcast, whose payload the relay stamps
// with the sender's identity (internal/relay/broadcast.go's rewriteVolatile)
// — so CollaboratorPayload models that MOUSE_LOCATION payload, and
// RoomPresenceEntry models the room-user-change roster row.

import type { Socket } from 'socket.io-client'
import type { AppState, BinaryFileData } from '@excalidraw/excalidraw/types'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'

/**
 * Live pointer state, carried inside a MOUSE_LOCATION client-broadcast
 * payload (see SceneBroadcastMessage below). The relay overwrites `user`
 * with the sender's session identity, so a client must trust it.
 */
export interface CollaboratorPayload {
	user: { id: string; name: string }
	pointer: { x: number; y: number; tool: 'pointer' | 'laser' }
	button: 'down' | 'up'
	selectedElementIds: AppState['selectedElementIds']
}

/** One roster row of a `room-user-change` event: presence only, no pointer. */
export interface RoomPresenceEntry {
	socketId: string
	user: { id: string; name: string }
	userId: string
	socketIds: string[]
}

export interface ServerToClientEvents {
	'init-room': () => void
	'room-user-change': (entries: RoomPresenceEntry[]) => void
	'user-joined': (data: { userId: string; userName: string; socketId: string; isSyncer: boolean }) => void
	'sync-designate': (data: { isSyncer: boolean }) => void
	'client-broadcast': (payload: ArrayBuffer, iv?: ArrayBuffer | number[]) => void
	'error': (message: string) => void
	/**
	 * The server pushes this whenever internal/room's Manager enters or
	 * clears a conflict for this room — a foreign WOPI lock, or a version
	 * drift, detected outside this client's own session — or a dirty
	 * room's save loop has failed for a while (saveStalled, a distinct
	 * condition from a conflict). It is a server-initiated broadcast
	 * (internal/relay.Relay.BroadcastToRoom), not a reply to any client
	 * emit; GET /api/board/conflict answers the same shape as a poll
	 * fallback and for the state on load.
	 */
	'conflict-state': (data: { inConflict: boolean; saveStalled: boolean }) => void
	/**
	 * The server pushes this to every client in the room once a writer
	 * resolves a conflict on the reload branch (internal/room's
	 * Manager.ResolveConflict, overwrite=false). The room's retained scene
	 * is gone server-side at that point, so every client, not just the
	 * one that resolved it, must reload to pick up the host's current
	 * content.
	 */
	'reload-required': () => void
}

export interface ClientToServerEvents {
	'join-room': (roomId: string) => void
	'server-broadcast': (roomId: string, payload: ArrayBuffer | Uint8Array, iv: ArrayBuffer | number[] | []) => void
	'server-volatile-broadcast': (roomId: string, payload: Uint8Array) => void
	'image-get': (roomId: string, id: string) => void
}

export type CollaborationSocket = Socket<ServerToClientEvents, ClientToServerEvents>

/**
 * Payload envelope carried inside the ArrayBuffer bytes of a
 * client-broadcast/server-broadcast/server-volatile-broadcast event. The
 * relay forwards these bytes untouched; only the client interprets the
 * `type` tag.
 */
export type SceneBroadcastMessage =
	| { type: 'SCENE_INIT'; payload: { elements: readonly ExcalidrawElement[] } }
	| { type: 'SCENE_UPDATE'; payload: { elements: readonly ExcalidrawElement[] } }
	| { type: 'MOUSE_LOCATION'; payload: CollaboratorPayload }
	| { type: 'IMAGE_ADD'; payload: { file: BinaryFileData } }
	| { type: 'IMAGE_REQUEST'; payload: { fileId: string } }
