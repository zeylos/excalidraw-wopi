/**
 * SPDX-FileCopyrightText: 2024 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// 'unavailable' is distinct from 'ignore': both resolve to an empty scene
// today, but 'unavailable' means the server fetch itself failed (a network
// error, a 5xx) rather than confirming the board is genuinely empty (a 404
// with no local fallback). Callers must not persist an 'unavailable' scene as
// the confirmed board state — doing so risks later overwriting the WOPI host
// with an empty scene once a save-back path exists.
export type LocalBoardDataPolicy = 'reconcile' | 'fallback' | 'ignore' | 'unavailable'

export function getLocalBoardDataPolicy(
	hasServerData: boolean,
	hasLocalData: boolean,
	hasPendingLocalChanges: boolean,
	isReadOnly: boolean,
	serverFetchFailed = false,
): LocalBoardDataPolicy {
	if (isReadOnly) {
		return 'ignore'
	}
	if (hasServerData && hasLocalData && hasPendingLocalChanges) {
		return 'reconcile'
	}
	if (!hasServerData && hasLocalData) {
		return 'fallback'
	}
	if (serverFetchFailed) {
		return 'unavailable'
	}
	return 'ignore'
}
