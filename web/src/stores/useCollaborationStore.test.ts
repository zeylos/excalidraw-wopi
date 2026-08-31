// SPDX-License-Identifier: AGPL-3.0-or-later

import { beforeEach, describe, expect, it } from 'vitest'
import { useCollaborationStore } from './useCollaborationStore'

beforeEach(() => {
	useCollaborationStore.getState().resetStore()
	// resetStore deliberately leaves a committed reload flag set (it is
	// terminal in production; see the `reloading` field's own comment), so a
	// test that calls setReloading(true) would otherwise leak it into the
	// next test. Force full isolation here instead.
	useCollaborationStore.setState({ reloading: false })
})

describe('useCollaborationStore markTerminalAuthFailure', () => {
	it('marks isPersistent true on the very first call: there is no refresh path to wait out', () => {
		useCollaborationStore.getState().markTerminalAuthFailure('jwt_secret_mismatch', 'no refresh path')

		const { authError } = useCollaborationStore.getState()
		expect(authError.isPersistent).toBe(true)
		expect(authError.type).toBe('jwt_secret_mismatch')
		expect(authError.message).toBe('no refresh path')
	})

	it('clearAuthError resets to the initial auth-error state', () => {
		useCollaborationStore.getState().markTerminalAuthFailure('jwt_secret_mismatch', 'no refresh path')
		expect(useCollaborationStore.getState().authError.isPersistent).toBe(true)

		useCollaborationStore.getState().clearAuthError()

		expect(useCollaborationStore.getState().authError).toEqual({
			type: null,
			message: null,
			isPersistent: false,
		})
	})
})

describe('useCollaborationStore basic state', () => {
	it('setStatus updates the connection status', () => {
		useCollaborationStore.getState().setStatus('connecting')
		expect(useCollaborationStore.getState().status).toBe('connecting')

		useCollaborationStore.getState().setStatus('online')
		expect(useCollaborationStore.getState().status).toBe('online')
	})

	it('tracks isDedicatedSyncer and isInRoom independently', () => {
		useCollaborationStore.getState().setDedicatedSyncer(true)
		useCollaborationStore.getState().setIsInRoom(true)

		expect(useCollaborationStore.getState().isDedicatedSyncer).toBe(true)
		expect(useCollaborationStore.getState().isInRoom).toBe(true)
	})
})

describe('useCollaborationStore conflict flag', () => {
	it('starts false', () => {
		expect(useCollaborationStore.getState().conflict).toBe(false)
	})

	it('setConflict(true) flips it on', () => {
		useCollaborationStore.getState().setConflict(true)
		expect(useCollaborationStore.getState().conflict).toBe(true)
	})

	it('setConflict(false) flips it back off', () => {
		useCollaborationStore.getState().setConflict(true)
		useCollaborationStore.getState().setConflict(false)
		expect(useCollaborationStore.getState().conflict).toBe(false)
	})

	it('resetStore clears an in-progress conflict', () => {
		useCollaborationStore.getState().setConflict(true)
		useCollaborationStore.getState().resetStore()
		expect(useCollaborationStore.getState().conflict).toBe(false)
	})
})

describe('useCollaborationStore saveStalled flag', () => {
	it('starts false', () => {
		expect(useCollaborationStore.getState().saveStalled).toBe(false)
	})

	it('setSaveStalled(true) flips it on', () => {
		useCollaborationStore.getState().setSaveStalled(true)
		expect(useCollaborationStore.getState().saveStalled).toBe(true)
	})

	it('setSaveStalled(false) flips it back off', () => {
		useCollaborationStore.getState().setSaveStalled(true)
		useCollaborationStore.getState().setSaveStalled(false)
		expect(useCollaborationStore.getState().saveStalled).toBe(false)
	})

	it('resetStore clears an in-progress stall', () => {
		useCollaborationStore.getState().setSaveStalled(true)
		useCollaborationStore.getState().resetStore()
		expect(useCollaborationStore.getState().saveStalled).toBe(false)
	})
})

describe('useCollaborationStore reloading flag', () => {
	it('starts false', () => {
		expect(useCollaborationStore.getState().reloading).toBe(false)
	})

	it('setReloading(true) flips it on, ahead of the reload it guards', () => {
		useCollaborationStore.getState().setReloading(true)
		expect(useCollaborationStore.getState().reloading).toBe(true)
	})

	it('resetStore does NOT clear a committed reload flag: it is terminal, and an unmount-time reset must not re-open the unload PUT gate', () => {
		useCollaborationStore.getState().setReloading(true)
		useCollaborationStore.getState().resetStore()
		expect(useCollaborationStore.getState().reloading).toBe(true)
	})
})
