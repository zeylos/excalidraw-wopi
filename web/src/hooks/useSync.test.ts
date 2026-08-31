// SPDX-License-Identifier: AGPL-3.0-or-later

import { beforeEach, describe, expect, it } from 'vitest'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import { shouldSkipFinalServerSync, shouldSkipLocalSync, shouldSkipServerAPISync } from './useSync'
import type { ServerAPISyncGateOptions } from './useSync'

beforeEach(() => {
	useCollaborationStore.getState().resetStore()
	// resetStore deliberately leaves a committed reload flag set (it is
	// terminal in production; see useCollaborationStore's `reloading` field),
	// so a test that calls setReloading(true) would otherwise leak it into
	// the next test. Force full isolation here instead.
	useCollaborationStore.setState({ reloading: false })
})

// A full set of gate-open options, so each test below only overrides the
// one field it means to exercise.
const baseGateOptions: ServerAPISyncGateOptions = {
	forceSync: false,
	isWorkerReady: true,
	hasWorker: true,
	fileId: 'file-1',
	hasExcalidrawAPI: true,
	isDedicatedSyncer: true,
	isReadOnly: false,
	collabStatus: 'online',
	conflict: false,
	reloading: false,
	initialDataResolved: true,
}

describe('shouldSkipFinalServerSync', () => {
	it('allows the final sync for a dedicated syncer with no conflict, no pending reload, and resolved initial data', () => {
		expect(shouldSkipFinalServerSync('file-1', true, false, false, true)).toBe(false)
	})

	it('skips when the tab is not the dedicated syncer', () => {
		expect(shouldSkipFinalServerSync('file-1', false, false, false, true)).toBe(true)
	})

	it('skips when the room is in conflict', () => {
		expect(shouldSkipFinalServerSync('file-1', true, true, false, true)).toBe(true)
	})

	it('skips once this tab has committed to a reload, even with no conflict', () => {
		expect(shouldSkipFinalServerSync('file-1', true, false, true, true)).toBe(true)
	})

	it('skips with no fileId', () => {
		expect(shouldSkipFinalServerSync(null, true, false, false, true)).toBe(true)
	})

	it('skips while the initial board data has not resolved yet, even with every other gate open', () => {
		expect(shouldSkipFinalServerSync('file-1', true, false, false, false)).toBe(true)
	})
})

describe('useCollaborationStore.reloading gates shouldSkipFinalServerSync', () => {
	it('the pre-reload flip flips the gate from allow to skip, closing the ConflictBanner reload race', () => {
		const fileId = 'file-1'
		const isSyncer = true
		const conflict = false

		expect(shouldSkipFinalServerSync(fileId, isSyncer, conflict, useCollaborationStore.getState().reloading, true)).toBe(false)

		// The exact call ConflictBanner's handleReload and useCollaboration's
		// reload-required handler make before window.location.reload().
		useCollaborationStore.getState().setReloading(true)

		expect(shouldSkipFinalServerSync(fileId, isSyncer, conflict, useCollaborationStore.getState().reloading, true)).toBe(true)
	})
})

describe('shouldSkipLocalSync', () => {
	it('allows the local sync with every gate open', () => {
		expect(shouldSkipLocalSync(true, true, 'file-1', true, false, false)).toBe(false)
	})

	it('skips once this tab has committed to a reload, even with every other gate open', () => {
		expect(shouldSkipLocalSync(true, true, 'file-1', true, false, true)).toBe(true)
	})

	it('skips a read-only session', () => {
		expect(shouldSkipLocalSync(true, true, 'file-1', true, true, false)).toBe(true)
	})

	it('skips with no fileId', () => {
		expect(shouldSkipLocalSync(true, true, null, true, false, false)).toBe(true)
	})

	it('skips while the worker is not ready', () => {
		expect(shouldSkipLocalSync(false, true, 'file-1', true, false, false)).toBe(true)
	})
})

describe('useCollaborationStore.reloading gates shouldSkipLocalSync', () => {
	it('the pre-reload flip flips the gate from allow to skip, closing the discard-then-resurrect race', () => {
		expect(shouldSkipLocalSync(true, true, 'file-1', true, false, useCollaborationStore.getState().reloading)).toBe(false)

		// The exact call ConflictBanner's handleReload and useCollaboration's
		// reload-required handler make before window.location.reload().
		useCollaborationStore.getState().setReloading(true)

		expect(shouldSkipLocalSync(true, true, 'file-1', true, false, useCollaborationStore.getState().reloading)).toBe(true)
	})
})

describe('shouldSkipServerAPISync', () => {
	it('allows the normal-path sync for a dedicated syncer, online, with every gate open', () => {
		expect(shouldSkipServerAPISync(baseGateOptions)).toBe(false)
	})

	it('allows a force sync with no dedicated-syncer or collabStatus requirement', () => {
		expect(shouldSkipServerAPISync({
			...baseGateOptions, forceSync: true, isDedicatedSyncer: false, collabStatus: 'offline',
		})).toBe(false)
	})

	it('skips the normal-path sync once this tab has committed to a reload', () => {
		expect(shouldSkipServerAPISync({ ...baseGateOptions, reloading: true })).toBe(true)
	})

	it('skips a force sync once this tab has committed to a reload', () => {
		expect(shouldSkipServerAPISync({ ...baseGateOptions, forceSync: true, reloading: true })).toBe(true)
	})
})

describe('useCollaborationStore.reloading gates shouldSkipServerAPISync', () => {
	it('the pre-reload flip flips the gate from allow to skip, closing the throttled-trailing-edge reload race', () => {
		expect(shouldSkipServerAPISync({ ...baseGateOptions, reloading: useCollaborationStore.getState().reloading })).toBe(false)

		// The exact call ConflictBanner's handleReload and useCollaboration's
		// reload-required handler make before window.location.reload().
		useCollaborationStore.getState().setReloading(true)

		expect(shouldSkipServerAPISync({ ...baseGateOptions, reloading: useCollaborationStore.getState().reloading })).toBe(true)
	})
})
