/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// This project has no attribution, table, or AI-disclosure feature, so
// every related customData field (creator, creatorProof, lastModifiedBy,
// aiGenerated, aiDisclosureLabel, isTable, tableHtml, tableLock) is unused.
// What remains is a trivial pass-through of stock reconcileElements.
// prepareDuplicatedElements is unused too: its only job was to strip an
// attribution stamp on duplicate, through the also-unused
// beforeElementCreated prop.

import { reconcileElements } from '@excalidraw/excalidraw'
import type { ExcalidrawElement, OrderedExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { RemoteExcalidrawElement } from '@excalidraw/excalidraw/data/reconcile'
import type { AppState } from '@excalidraw/excalidraw/types'

export function mergeElementsWithMetadata(
	localElements: readonly OrderedExcalidrawElement[],
	remoteElements: readonly RemoteExcalidrawElement[],
	appState: AppState,
): ExcalidrawElement[] {
	return reconcileElements(localElements, remoteElements, appState)
}
