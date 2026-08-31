// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { decodeClientBroadcast } from './decodeClientBroadcast'

function encode(value: unknown): ArrayBuffer {
	return new TextEncoder().encode(JSON.stringify(value)).buffer as ArrayBuffer
}

describe('decodeClientBroadcast', () => {
	it('returns null for malformed JSON', () => {
		expect(decodeClientBroadcast(new TextEncoder().encode('{not json').buffer as ArrayBuffer)).toBeNull()
	})

	it('returns null for a missing type', () => {
		expect(decodeClientBroadcast(encode({ payload: {} }))).toBeNull()
	})

	it('returns null for an unknown type', () => {
		expect(decodeClientBroadcast(encode({ type: 'NOT_A_TYPE', payload: {} }))).toBeNull()
	})

	it('returns null for a missing payload', () => {
		expect(decodeClientBroadcast(encode({ type: 'SCENE_INIT' }))).toBeNull()
	})

	it.each(['SCENE_INIT', 'SCENE_UPDATE'] as const)('decodes a valid %s with an elements array', (type) => {
		const elements = [{ id: 'a' }]
		const result = decodeClientBroadcast(encode({ type, payload: { elements } }))
		expect(result).toEqual({ type, payload: { elements } })
	})

	it.each(['SCENE_INIT', 'SCENE_UPDATE'] as const)('rejects a %s payload whose elements is not an array', (type) => {
		expect(decodeClientBroadcast(encode({ type, payload: { elements: 'nope' } }))).toBeNull()
	})

	it('rejects a SCENE_RESTORE: dead code, nothing emits it', () => {
		expect(decodeClientBroadcast(encode({ type: 'SCENE_RESTORE', payload: { reloadFromServer: true } }))).toBeNull()
	})

	it('decodes a valid MOUSE_LOCATION, trusting the server-stamped user', () => {
		const payload = {
			user: { id: 'u1', name: 'Alice' },
			pointer: { x: 1, y: 2, tool: 'pointer' },
			button: 'down',
			selectedElementIds: {},
		}
		const result = decodeClientBroadcast(encode({ type: 'MOUSE_LOCATION', payload }))
		expect(result).toEqual({ type: 'MOUSE_LOCATION', payload })
	})

	it('rejects a MOUSE_LOCATION missing pointer or button', () => {
		expect(decodeClientBroadcast(encode({ type: 'MOUSE_LOCATION', payload: { user: { id: 'u1', name: 'Alice' } } }))).toBeNull()
	})

	it('rejects a MOUSE_LOCATION with a non-string user.id', () => {
		const payload = { user: { id: 42, name: 'Alice' }, pointer: { x: 1, y: 2, tool: 'pointer' }, button: 'down' }
		expect(decodeClientBroadcast(encode({ type: 'MOUSE_LOCATION', payload }))).toBeNull()
	})

	it('rejects a MOUSE_LOCATION with a non-string user.name', () => {
		const payload = { user: { id: 'u1', name: { toString: () => 'Alice' } }, pointer: { x: 1, y: 2, tool: 'pointer' }, button: 'down' }
		expect(decodeClientBroadcast(encode({ type: 'MOUSE_LOCATION', payload }))).toBeNull()
	})

	it('rejects a MOUSE_LOCATION with no user at all', () => {
		const payload = { pointer: { x: 1, y: 2, tool: 'pointer' }, button: 'down' }
		expect(decodeClientBroadcast(encode({ type: 'MOUSE_LOCATION', payload }))).toBeNull()
	})

	it('decodes a valid IMAGE_ADD', () => {
		const file = { id: 'f1', dataURL: 'data:image/png;base64,abc', mimeType: 'image/png' }
		const result = decodeClientBroadcast(encode({ type: 'IMAGE_ADD', payload: { file } }))
		expect(result).toEqual({ type: 'IMAGE_ADD', payload: { file } })
	})

	it('rejects an IMAGE_ADD without a file', () => {
		expect(decodeClientBroadcast(encode({ type: 'IMAGE_ADD', payload: {} }))).toBeNull()
	})

	it('rejects an IMAGE_ADD whose dataURL is not a data: URI', () => {
		const file = { id: 'f1', dataURL: 'https://evil.example/track.png', mimeType: 'image/png' }
		expect(decodeClientBroadcast(encode({ type: 'IMAGE_ADD', payload: { file } }))).toBeNull()
	})

	it('rejects an IMAGE_ADD whose dataURL is a non-image data: URI', () => {
		const file = { id: 'f1', dataURL: 'data:text/html,<script>alert(1)</script>', mimeType: 'image/png' }
		expect(decodeClientBroadcast(encode({ type: 'IMAGE_ADD', payload: { file } }))).toBeNull()
	})

	it('rejects an IMAGE_ADD with a missing dataURL', () => {
		const file = { id: 'f1', mimeType: 'image/png' }
		expect(decodeClientBroadcast(encode({ type: 'IMAGE_ADD', payload: { file } }))).toBeNull()
	})

	it('decodes a valid IMAGE_REQUEST', () => {
		const result = decodeClientBroadcast(encode({ type: 'IMAGE_REQUEST', payload: { fileId: 'f1' } }))
		expect(result).toEqual({ type: 'IMAGE_REQUEST', payload: { fileId: 'f1' } })
	})

	it('rejects an IMAGE_REQUEST with a non-string fileId', () => {
		expect(decodeClientBroadcast(encode({ type: 'IMAGE_REQUEST', payload: { fileId: 1 } }))).toBeNull()
	})

	it('rejects a VIEWPORT_UPDATE: dead code, nothing emits it', () => {
		const payload = { userId: 'u1', scrollX: 1, scrollY: 2, zoom: 1.5 }
		expect(decodeClientBroadcast(encode({ type: 'VIEWPORT_UPDATE', payload }))).toBeNull()
	})
})
