// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { getLocalBoardDataPolicy } from './localBoardData'

describe('getLocalBoardDataPolicy', () => {
	it('ignores local data for a read-only session, regardless of other flags', () => {
		expect(getLocalBoardDataPolicy(true, true, true, true)).toBe('ignore')
		expect(getLocalBoardDataPolicy(false, false, false, true)).toBe('ignore')
		expect(getLocalBoardDataPolicy(true, false, false, true)).toBe('ignore')
	})

	it('reconciles when the server has data, local has data, and local has pending changes', () => {
		expect(getLocalBoardDataPolicy(true, true, true, false)).toBe('reconcile')
	})

	it('falls back to local data when the server has none but local does', () => {
		expect(getLocalBoardDataPolicy(false, true, false, false)).toBe('fallback')
		expect(getLocalBoardDataPolicy(false, true, true, false)).toBe('fallback')
	})

	it('ignores local data when the server has data and local has no pending changes', () => {
		expect(getLocalBoardDataPolicy(true, true, false, false)).toBe('ignore')
	})

	it('ignores when there is no local data at all', () => {
		expect(getLocalBoardDataPolicy(true, false, false, false)).toBe('ignore')
		expect(getLocalBoardDataPolicy(false, false, false, false)).toBe('ignore')
	})

	it('reports unavailable when the server fetch failed and no local data exists', () => {
		expect(getLocalBoardDataPolicy(false, false, false, false, true)).toBe('unavailable')
	})

	it('still falls back to local data when the server fetch failed but local data exists', () => {
		expect(getLocalBoardDataPolicy(false, true, false, false, true)).toBe('fallback')
	})

	it('ignores a failed fetch for a read-only session, same as any other case', () => {
		expect(getLocalBoardDataPolicy(false, false, false, true, true)).toBe('ignore')
	})
})
