// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { sanitizeAppStateForSync } from './sanitizeAppState'

describe('sanitizeAppStateForSync', () => {
	it('returns an empty object for null or undefined input', () => {
		expect(sanitizeAppStateForSync(null)).toEqual({})
		expect(sanitizeAppStateForSync(undefined)).toEqual({})
	})

	it('strips collaborators, selectedElementIds, width, height, and offsets', () => {
		const state = {
			collaborators: new Map([['u1', {}]]),
			selectedElementIds: { a: true },
			width: 800,
			height: 600,
			offsetTop: 10,
			offsetLeft: 20,
			viewBackgroundColor: '#fff',
		}
		const result = sanitizeAppStateForSync(state as never)
		expect(result).not.toHaveProperty('collaborators')
		expect(result).not.toHaveProperty('selectedElementIds')
		expect(result).not.toHaveProperty('width')
		expect(result).not.toHaveProperty('height')
		expect(result).not.toHaveProperty('offsetTop')
		expect(result).not.toHaveProperty('offsetLeft')
	})

	it('keeps every other key untouched', () => {
		const state = {
			viewBackgroundColor: '#fff',
			zoom: { value: 1.5 },
			scrollX: 42,
			scrollY: -7,
		}
		const result = sanitizeAppStateForSync(state as never)
		expect(result).toEqual(state)
	})

	it('does not mutate the input object', () => {
		const state = { width: 800, viewBackgroundColor: '#fff' }
		sanitizeAppStateForSync(state as never)
		expect(state).toHaveProperty('width')
	})
})
