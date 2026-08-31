// SPDX-License-Identifier: AGPL-3.0-or-later

import { beforeEach, describe, expect, it } from 'vitest'
import { useWhiteboardConfigStore } from './useWhiteboardConfigStore'

beforeEach(() => {
	useWhiteboardConfigStore.getState().resetStore()
})

describe('useWhiteboardConfigStore initialDataResolved flag', () => {
	it('starts false, so no server sync can fire before the first board load resolves', () => {
		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(false)
	})

	it('resolveInitialData flips it true', () => {
		useWhiteboardConfigStore.getState().resolveInitialData({ elements: [], files: {}, appState: {} })

		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(true)
	})

	it('resetInitialDataPromise flips it back false, gating the next board load', () => {
		useWhiteboardConfigStore.getState().resolveInitialData({ elements: [], files: {}, appState: {} })
		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(true)

		useWhiteboardConfigStore.getState().resetInitialDataPromise()

		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(false)
	})

	it('resetStore also flips it back false', () => {
		useWhiteboardConfigStore.getState().resolveInitialData({ elements: [], files: {}, appState: {} })
		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(true)

		useWhiteboardConfigStore.getState().resetStore()

		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(false)
	})

	it('resolveInitialData(data, false) resolves the promise but leaves initialDataResolved false, for a failed load', async () => {
		const promise = useWhiteboardConfigStore.getState().initialDataPromise

		useWhiteboardConfigStore.getState().resolveInitialData({ elements: [], files: {}, appState: {} }, false)

		await expect(promise).resolves.toEqual({ elements: [], files: {}, appState: {} })
		expect(useWhiteboardConfigStore.getState().initialDataResolved).toBe(false)
	})
})
