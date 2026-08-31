/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// fileId is an opaque host-issued string (a UUID in Drive), so the primary
// key is a plain `id` rather than an auto-increment integer. The schema is
// declared directly as version(1): no released build ever wrote a row under
// a different schema, so there is no earlier version to migrate from.

import * as Dexie from 'dexie'
import type { Table } from 'dexie'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState, BinaryFiles } from '@excalidraw/excalidraw/types'

export interface WhiteboardData {
	id: string
	elements: ExcalidrawElement[]
	files: BinaryFiles
	appState?: AppState
	savedAt?: number
	hasPendingLocalChanges?: boolean
	lastSyncedHash?: number
}

export class WhiteboardDatabase extends Dexie.Dexie {

	whiteboards!: Table<WhiteboardData>

	constructor() {
		super('WhiteboardDatabase')

		this.version(1).stores({
			whiteboards: 'id, savedAt',
		})
	}

	async get(
		fileId: string,
	): Promise<WhiteboardData | undefined> {
		return this.whiteboards.get(fileId)
	}

	// A reload that discards this session's edits (a conflict-resolution
	// Reload, or the server's reload-required broadcast) must clear this
	// flag first: otherwise the reloaded page's own load sees a stale
	// hasPendingLocalChanges:true row, reconciles the discarded local
	// elements back in (useBoardDataManager's 'reconcile' policy), and
	// re-saves them to the host. update() is a no-op if the row is gone.
	async clearPendingLocalChanges(fileId: string): Promise<void> {
		await this.whiteboards.update(fileId, { hasPendingLocalChanges: false })
	}

	async put(
		fileId: string,
		elements: ExcalidrawElement[],
		files: BinaryFiles,
		appState?: AppState,
		options: {
			hasPendingLocalChanges?: boolean
			lastSyncedHash?: number
		} = {},
	): Promise<string> {
		// The get-then-put below must run as one atomic unit. A
		// plain sequential await lets a concurrent write interleave between
		// the get and the put. The loser then reads a stale `existing` value,
		// and can mark a local edit as synced (or drop
		// hasPendingLocalChanges) when it never actually reached the server.
		// Dexie queues a same-table 'rw' transaction behind any other
		// transaction already touching that table, so two concurrent put()
		// calls now run one after the other, never interleaved.
		return this.transaction('rw', this.whiteboards, async () => {
			const existing = await this.whiteboards.get(fileId)

			const data = {
				id: fileId,
				elements,
				files,
				appState,
				savedAt: Date.now(),
				hasPendingLocalChanges: options.hasPendingLocalChanges ?? existing?.hasPendingLocalChanges ?? false,
				lastSyncedHash: options.lastSyncedHash ?? existing?.lastSyncedHash,
			}

			return this.whiteboards.put(data)
		})
	}

}

export const db = new WhiteboardDatabase()
