// SPDX-License-Identifier: AGPL-3.0-or-later

import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchWhiteboardSnapshot, WhiteboardSnapshotFetchError } from './fetchWhiteboardSnapshot'

const jsonResponse = (body: unknown, init: { status?: number } = {}) => ({
	ok: (init.status ?? 200) < 400,
	status: init.status ?? 200,
	text: () => Promise.resolve(body === undefined ? '' : JSON.stringify(body)),
} as Response)

afterEach(() => {
	vi.restoreAllMocks()
	vi.unstubAllGlobals()
})

describe('fetchWhiteboardSnapshot', () => {
	it('returns the snapshot for a successful response', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
			jsonResponse({ elements: [{ id: '1' }], files: {}, appState: {} }),
		))

		const snapshot = await fetchWhiteboardSnapshot('/api', 'token', 'file-1')
		expect(snapshot).toEqual({ elements: [{ id: '1' }], files: {}, appState: {} })
	})

	it('sends the room query parameter, URL-encoded', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ elements: [] }))
		vi.stubGlobal('fetch', fetchMock)

		await fetchWhiteboardSnapshot('/api', 'token', 'file/with spaces')

		expect(fetchMock.mock.calls[0][0]).toBe('/api/board?room=file%2Fwith%20spaces')
	})

	it('returns null on a 404: the board is genuinely empty, not a failure', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(null, { status: 404 })))

		const snapshot = await fetchWhiteboardSnapshot('/api', 'token', 'file-1')
		expect(snapshot).toBeNull()
	})

	it('returns null on a 0-byte 200: a WOPI host serves a new board as an empty file', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(undefined)))

		const snapshot = await fetchWhiteboardSnapshot('/api', 'token', 'file-1')
		expect(snapshot).toBeNull()
	})

	it('throws WhiteboardSnapshotFetchError on a network error', async () => {
		vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('network down')))

		await expect(fetchWhiteboardSnapshot('/api', 'token', 'file-1')).rejects.toBeInstanceOf(WhiteboardSnapshotFetchError)
	})

	it('throws WhiteboardSnapshotFetchError on a non-404 error status', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(null, { status: 500 })))

		await expect(fetchWhiteboardSnapshot('/api', 'token', 'file-1')).rejects.toBeInstanceOf(WhiteboardSnapshotFetchError)
	})

	it('throws WhiteboardSnapshotFetchError on a body with no elements array', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ notElements: true })))

		await expect(fetchWhiteboardSnapshot('/api', 'token', 'file-1')).rejects.toBeInstanceOf(WhiteboardSnapshotFetchError)
	})
})
