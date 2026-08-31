// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { useSessionStore } from './useSessionStore'

describe('useSessionStore', () => {
	it('has no session before setSession is called', () => {
		expect(useSessionStore.getState().getToken()).toBeNull()
		expect(useSessionStore.getState().isReadOnly).toBe(false)
	})

	it('sets the token and identity from an AppConfig-shaped payload', () => {
		useSessionStore.getState().setSession({
			sessionToken: 'a.b.c',
			userId: 'user-1',
			userName: 'Alice',
			canWrite: true,
		})

		const state = useSessionStore.getState()
		expect(state.getToken()).toBe('a.b.c')
		expect(state.userId).toBe('user-1')
		expect(state.userName).toBe('Alice')
		expect(state.canWrite).toBe(true)
		expect(state.isReadOnly).toBe(false)
	})

	it('derives isReadOnly from canWrite=false', () => {
		useSessionStore.getState().setSession({
			sessionToken: 'a.b.c',
			userId: 'user-1',
			userName: 'Alice',
			canWrite: false,
		})

		expect(useSessionStore.getState().isReadOnly).toBe(true)
	})
})
