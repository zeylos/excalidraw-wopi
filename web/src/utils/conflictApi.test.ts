// SPDX-License-Identifier: AGPL-3.0-or-later

import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchConflictState, resolveConflict } from './conflictApi'

const jsonResponse = (body: unknown, init: { status?: number } = {}) => ({
	ok: (init.status ?? 200) < 400,
	status: init.status ?? 200,
	json: () => Promise.resolve(body),
} as Response)

afterEach(() => {
	vi.restoreAllMocks()
	vi.unstubAllGlobals()
})

describe('fetchConflictState', () => {
	it('returns the conflict state when the server reports a conflict', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ inConflict: true, saveStalled: false })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toEqual({ inConflict: true, saveStalled: false })
	})

	it('returns the conflict state when the server reports no conflict', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ inConflict: false, saveStalled: false })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toEqual({ inConflict: false, saveStalled: false })
	})

	it('returns the conflict state when the server reports a stalled save', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ inConflict: false, saveStalled: true })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toEqual({ inConflict: false, saveStalled: true })
	})

	it('sends the bearer token and the room query parameter', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ inConflict: false, saveStalled: false }))
		vi.stubGlobal('fetch', fetchMock)

		await fetchConflictState('/api', 'the-token', 'file-1')

		expect(fetchMock).toHaveBeenCalledWith('/api/board/conflict?room=file-1', {
			method: 'GET',
			headers: { Authorization: 'Bearer the-token' },
		})
	})

	it('URL-encodes the fileId in the room query parameter', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse({ inConflict: false, saveStalled: false }))
		vi.stubGlobal('fetch', fetchMock)

		await fetchConflictState('/api', 'the-token', 'file/with spaces')

		expect(fetchMock).toHaveBeenCalledWith('/api/board/conflict?room=file%2Fwith%20spaces', expect.anything())
	})

	it('returns null on a network error', async () => {
		vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('network down')))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toBeNull()
	})

	it('returns null on a non-2xx status', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(null, { status: 500 })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toBeNull()
	})

	it('returns null on a body with no boolean inConflict field', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ inConflict: 'yes', saveStalled: false })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toBeNull()
	})

	it('returns null on a body with no boolean saveStalled field', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ inConflict: false })))

		await expect(fetchConflictState('/api', 'token', 'file-1')).resolves.toBeNull()
	})
})

describe('resolveConflict', () => {
	it('posts overwrite:true and returns true on success', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse(null, { status: 204 }))
		vi.stubGlobal('fetch', fetchMock)

		await expect(resolveConflict('/api', 'the-token', 'file-1', true)).resolves.toBe(true)

		expect(fetchMock).toHaveBeenCalledWith('/api/board/conflict/resolve?room=file-1', {
			method: 'POST',
			headers: {
				Authorization: 'Bearer the-token',
				'Content-Type': 'application/json',
			},
			body: JSON.stringify({ overwrite: true }),
		})
	})

	it('posts overwrite:false', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse(null, { status: 204 }))
		vi.stubGlobal('fetch', fetchMock)

		await resolveConflict('/api', 'token', 'file-1', false)

		const body = JSON.parse(fetchMock.mock.calls[0][1].body)
		expect(body).toEqual({ overwrite: false })
	})

	it('URL-encodes the fileId in the room query parameter', async () => {
		const fetchMock = vi.fn().mockResolvedValue(jsonResponse(null, { status: 204 }))
		vi.stubGlobal('fetch', fetchMock)

		await resolveConflict('/api', 'token', 'file/with spaces', true)

		expect(fetchMock.mock.calls[0][0]).toBe('/api/board/conflict/resolve?room=file%2Fwith%20spaces')
	})

	it('returns false on a non-2xx status', async () => {
		vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse(null, { status: 403 })))

		await expect(resolveConflict('/api', 'token', 'file-1', true)).resolves.toBe(false)
	})

	it('returns false on a network error', async () => {
		vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('network down')))

		await expect(resolveConflict('/api', 'token', 'file-1', true)).resolves.toBe(false)
	})
})
