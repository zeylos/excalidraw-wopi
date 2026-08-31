// SPDX-License-Identifier: AGPL-3.0-or-later

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'

const dbMock = {
	get: vi.fn(),
	put: vi.fn(),
}

vi.mock('../database/db', () => ({
	db: dbMock,
}))

// Imported after the db mock is registered, so handleSyncToLocal/handleSyncToServer
// pick up the mock instead of the real Dexie-backed database.
const { handleSyncToLocal, handleSyncToServer, handleMessage } = await import('./syncWorker')

function makeElement(id: string): ExcalidrawElement {
	return { id, type: 'rectangle', version: 1, versionNonce: 1, isDeleted: false } as unknown as ExcalidrawElement
}

beforeEach(() => {
	dbMock.get.mockReset().mockResolvedValue(undefined)
	dbMock.put.mockReset().mockResolvedValue('file-1')
	vi.stubGlobal('fetch', vi.fn())
})

afterEach(() => {
	vi.unstubAllGlobals()
})

describe('handleSyncToLocal', () => {
	it('writes the elements to the local database and reports success', async () => {
		const elements = [makeElement('a')]
		const result = await handleSyncToLocal({
			type: 'SYNC_TO_LOCAL',
			fileId: 'file-1',
			elements,
			files: {},
		})

		expect(dbMock.put).toHaveBeenCalledWith('file-1', elements, {}, undefined, { hasPendingLocalChanges: true })
		expect(result).toEqual({ type: 'LOCAL_SYNC_COMPLETE', duration: expect.any(Number), elementsCount: 1 })
	})

	it('strips collaborators off the appState before persisting', async () => {
		const collaborators = new Map()
		await handleSyncToLocal({
			type: 'SYNC_TO_LOCAL',
			fileId: 'file-1',
			elements: [],
			files: {},
			appState: { collaborators, zoom: { value: 1 } } as never,
		})

		const [, , , persistedAppState] = dbMock.put.mock.calls[0]
		expect(persistedAppState).toEqual({ zoom: { value: 1 } })
	})

	it('reports an error when fileId is missing', async () => {
		const result = await handleSyncToLocal({
			type: 'SYNC_TO_LOCAL',
			fileId: '',
			elements: [],
			files: {},
		})

		expect(result).toEqual({ type: 'LOCAL_SYNC_ERROR', error: 'Missing fileId for local sync' })
		expect(dbMock.put).not.toHaveBeenCalled()
	})

	it('reports an error when the database write throws', async () => {
		dbMock.put.mockRejectedValueOnce(new Error('quota exceeded'))
		const result = await handleSyncToLocal({
			type: 'SYNC_TO_LOCAL',
			fileId: 'file-1',
			elements: [],
			files: {},
		})

		expect(result).toEqual({ type: 'LOCAL_SYNC_ERROR', error: 'quota exceeded' })
	})
})

describe('handleSyncToServer', () => {
	const baseMessage = {
		type: 'SYNC_TO_SERVER' as const,
		fileId: 'file-1',
		url: 'https://example.test/api/board',
		jwt: 'token-abc',
		elements: [makeElement('a')],
		files: {},
	}

	it('PUTs the scene with a bearer token and writes back lastSyncedHash on success', async () => {
		const fetchMock = vi.mocked(fetch)
		fetchMock.mockResolvedValueOnce(new Response(JSON.stringify({ ok: true }), { status: 200 }))

		const result = await handleSyncToServer(baseMessage)

		expect(fetchMock).toHaveBeenCalledWith('https://example.test/api/board', {
			method: 'PUT',
			headers: {
				'Content-Type': 'application/json',
				Authorization: 'Bearer token-abc',
			},
			body: JSON.stringify({ elements: baseMessage.elements, files: {} }),
		})
		expect(dbMock.put).toHaveBeenCalledWith(
			'file-1',
			baseMessage.elements,
			{},
			undefined,
			expect.objectContaining({ hasPendingLocalChanges: false, lastSyncedHash: expect.any(Number) }),
		)
		expect(result).toMatchObject({ type: 'SERVER_SYNC_COMPLETE', success: true, elementsCount: 1 })
	})

	it('treats a 409 as a skipped, successful sync and does not touch the local database', async () => {
		const fetchMock = vi.mocked(fetch)
		fetchMock.mockResolvedValueOnce(new Response(null, { status: 409 }))

		const result = await handleSyncToServer(baseMessage)

		expect(result).toEqual({
			type: 'SERVER_SYNC_COMPLETE',
			success: true,
			skipped: true,
			duration: 0,
			elementsCount: 1,
		})
		expect(dbMock.put).not.toHaveBeenCalled()
	})

	it('reports an error when the server rejects the payload as too large (413)', async () => {
		const fetchMock = vi.mocked(fetch)
		fetchMock.mockResolvedValueOnce(new Response('Payload Too Large', { status: 413 }))

		const result = await handleSyncToServer(baseMessage)

		expect(result).toEqual({
			type: 'SERVER_SYNC_ERROR',
			error: 'Server responded with status: 413 - Payload Too Large',
		})
		expect(dbMock.put).not.toHaveBeenCalled()
	})

	it('reports an error when the token is missing', async () => {
		const result = await handleSyncToServer({ ...baseMessage, jwt: '' })
		expect(result).toEqual({ type: 'SERVER_SYNC_ERROR', error: 'Missing required data for server sync' })
	})

	it('reports an error when fetch itself rejects', async () => {
		const fetchMock = vi.mocked(fetch)
		fetchMock.mockRejectedValueOnce(new Error('network down'))

		const result = await handleSyncToServer(baseMessage)

		expect(result).toEqual({ type: 'SERVER_SYNC_ERROR', error: 'network down' })
	})
})

describe('handleMessage', () => {
	it('dispatches INIT to INIT_COMPLETE', async () => {
		expect(await handleMessage({ type: 'INIT' })).toEqual({ type: 'INIT_COMPLETE' })
	})

	it('dispatches SYNC_TO_LOCAL to handleSyncToLocal', async () => {
		const result = await handleMessage({ type: 'SYNC_TO_LOCAL', fileId: 'file-1', elements: [], files: {} })
		expect(result.type).toBe('LOCAL_SYNC_COMPLETE')
	})
})

// Without the message queue, the module's own top-level 'message' listener
// ran handleMessage(event.data) for every event concurrently, so a
// SYNC_TO_LOCAL landing while a SYNC_TO_SERVER's fetch was still in flight
// could finish its db.put() first, only for the SYNC_TO_SERVER handler's
// own later db.put() (built from its own, now-stale `elements`) to
// overwrite it — silently losing the local edit and marking it synced.
// This dispatches through the real, module-level
// `self.addEventListener('message', ...)` registered at import time (not
// through handleMessage directly), the same entry point the browser uses,
// to exercise the actual queueing.
describe('worker message queue serialization', () => {
	it('does not start a second dispatched message until the first settles', async () => {
		const order: string[] = []
		let releaseFirst: (() => void) | undefined
		const gate = new Promise<void>((resolve) => {
			releaseFirst = resolve
		})

		dbMock.put.mockImplementation(async (fileId: string) => {
			order.push(`put-start:${fileId}`)
			if (fileId === 'first') {
				await gate
			}
			order.push(`put-end:${fileId}`)
			return fileId
		})

		const postMessageSpy = vi.fn()
		vi.stubGlobal('postMessage', postMessageSpy)

		self.dispatchEvent(new MessageEvent('message', {
			data: { type: 'SYNC_TO_LOCAL', fileId: 'first', elements: [], files: {} },
		}))
		self.dispatchEvent(new MessageEvent('message', {
			data: { type: 'SYNC_TO_LOCAL', fileId: 'second', elements: [], files: {} },
		}))

		// Let queued microtasks run: the second message must still be
		// waiting on the first, which is itself blocked on `gate`.
		await Promise.resolve()
		await Promise.resolve()
		await Promise.resolve()
		expect(order).toEqual(['put-start:first'])

		releaseFirst?.()
		await vi.waitFor(() => expect(order).toContain('put-end:second'))

		expect(order).toEqual(['put-start:first', 'put-end:first', 'put-start:second', 'put-end:second'])
	})
})
