// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { shouldJoinRoom, shouldRetryJoin } from './roomJoin'

describe('shouldJoinRoom', () => {
	it('joins when no room has been joined yet', () => {
		expect(shouldJoinRoom(null, 'file-1')).toBe(true)
	})

	it('skips a repeat join for the same room', () => {
		expect(shouldJoinRoom('file-1', 'file-1')).toBe(false)
	})

	it('joins again when the target room differs from the tracked one', () => {
		expect(shouldJoinRoom('file-1', 'file-2')).toBe(true)
	})
})

describe('shouldRetryJoin', () => {
	it('retries while the attempt count is under the bound', () => {
		expect(shouldRetryJoin(0, 3)).toBe(true)
		expect(shouldRetryJoin(2, 3)).toBe(true)
	})

	it('stops retrying once the attempt count reaches the bound', () => {
		expect(shouldRetryJoin(3, 3)).toBe(false)
		expect(shouldRetryJoin(4, 3)).toBe(false)
	})
})
