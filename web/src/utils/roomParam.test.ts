// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { withRoomParam } from './roomParam'

describe('withRoomParam', () => {
	it('appends the room parameter to a bare url', () => {
		expect(withRoomParam('/api/board', 'file-1')).toBe('/api/board?room=file-1')
	})

	it('URL-encodes a fileId that needs encoding', () => {
		expect(withRoomParam('/api/board', 'file/with spaces')).toBe('/api/board?room=file%2Fwith%20spaces')
	})

	it('appends with an ampersand when the url already has a query string', () => {
		expect(withRoomParam('/api/board?foo=bar', 'file-1')).toBe('/api/board?foo=bar&room=file-1')
	})
})
