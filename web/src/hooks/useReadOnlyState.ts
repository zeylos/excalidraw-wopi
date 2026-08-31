/**
 * SPDX-FileCopyrightText: 2024 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// The JWT's canWrite claim is minted once, at launch, and
// useSessionStore.setSession already derives isReadOnly from it
// synchronously — nothing here needs to fetch or decode a token. This
// hook's only job is applying that value to the Excalidraw API's
// viewModeEnabled whenever it changes or whenever the API becomes
// available. It does not mirror the value onto useWhiteboardConfigStore:
// that store holds no read-only state, and useSessionStore.isReadOnly is
// this project's one source of truth.

import { useCallback, useEffect } from 'react'
import { useExcalidrawStore } from '../stores/useExcalidrawStore'
import { useSessionStore } from '../stores/useSessionStore'
import logger from '../utils/logger'

export function useReadOnlyState() {
	const excalidrawAPI = useExcalidrawStore(state => state.excalidrawAPI)
	const isReadOnly = useSessionStore(state => state.isReadOnly)

	const applyReadOnlyState = useCallback((readOnly: boolean) => {
		logger.debug('[Permissions] User has', readOnly ? 'read-only' : 'write', 'access')

		if (!excalidrawAPI) {
			return
		}

		try {
			const currentViewMode = excalidrawAPI.getAppState().viewModeEnabled

			if (readOnly && !currentViewMode) {
				excalidrawAPI.updateScene({ appState: { viewModeEnabled: true } })
			} else if (!readOnly && currentViewMode) {
				excalidrawAPI.updateScene({ appState: { viewModeEnabled: false } })
			}
		} catch (error) {
			logger.error('[Permissions] Error updating view mode via Excalidraw API:', error)
		}
	}, [excalidrawAPI])

	useEffect(() => {
		applyReadOnlyState(isReadOnly)
	}, [isReadOnly, applyReadOnlyState])

	return {
		isReadOnly,
		applyReadOnlyState,
	}
}
