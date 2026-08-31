// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { getConflictBannerState } from './ConflictBanner'

describe('getConflictBannerState', () => {
	it('is hidden when there is no conflict and no stalled save, for a writer', () => {
		expect(getConflictBannerState(false, false, false)).toEqual({ visible: false, showActions: false, showStalledReload: false })
	})

	it('is hidden when there is no conflict and no stalled save, for a read-only viewer', () => {
		expect(getConflictBannerState(false, false, true)).toEqual({ visible: false, showActions: false, showStalledReload: false })
	})

	it('shows actions for a writer in conflict', () => {
		expect(getConflictBannerState(true, false, false)).toEqual({ visible: true, showActions: true, showStalledReload: false })
	})

	it('is visible but hides actions for a read-only viewer in conflict', () => {
		expect(getConflictBannerState(true, false, true)).toEqual({ visible: true, showActions: false, showStalledReload: false })
	})

	it('shows the reload button for a stalled save, for a writer', () => {
		expect(getConflictBannerState(false, true, false)).toEqual({ visible: true, showActions: false, showStalledReload: true })
	})

	it('hides the reload button for a stalled save, for a read-only viewer, since its token cannot restore saving', () => {
		expect(getConflictBannerState(false, true, true)).toEqual({ visible: true, showActions: false, showStalledReload: false })
	})

	it('hides the stalled reload button when a conflict also holds, since conflict copy takes precedence', () => {
		expect(getConflictBannerState(true, true, false)).toEqual({ visible: true, showActions: true, showStalledReload: false })
	})

	it('hides the stalled reload button when a conflict also holds, for a read-only viewer', () => {
		expect(getConflictBannerState(true, true, true)).toEqual({ visible: true, showActions: false, showStalledReload: false })
	})
})
