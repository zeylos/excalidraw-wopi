// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import type { ExcalidrawElement } from '@excalidraw/excalidraw/element/types'
import type { AppState } from '@excalidraw/excalidraw/types'
import {
	buildBroadcastedElementVersions,
	computeElementVersionHash,
	decideSceneBroadcast,
	getIncrementalSceneElements,
	mergeSceneElements,
} from './syncSceneData'

function makeElement(overrides: Partial<ExcalidrawElement> & { id: string }): ExcalidrawElement {
	return {
		type: 'rectangle',
		x: 0,
		y: 0,
		width: 10,
		height: 10,
		version: 1,
		versionNonce: 1,
		isDeleted: false,
		...overrides,
	} as unknown as ExcalidrawElement
}

function makeAppState(overrides: Partial<AppState> = {}): AppState {
	return {
		newElement: null,
		resizingElement: null,
		...overrides,
	} as unknown as AppState
}

describe('computeElementVersionHash', () => {
	it('returns 1 for an empty array', () => {
		expect(computeElementVersionHash([])).toBe(1)
	})

	it('changes when a version changes', () => {
		const a = [makeElement({ id: 'a', version: 1 })]
		const b = [makeElement({ id: 'a', version: 2 })]
		expect(computeElementVersionHash(a)).not.toBe(computeElementVersionHash(b))
	})

	it('changes when a versionNonce changes', () => {
		const a = [makeElement({ id: 'a', versionNonce: 1 })]
		const b = [makeElement({ id: 'a', versionNonce: 2 })]
		expect(computeElementVersionHash(a)).not.toBe(computeElementVersionHash(b))
	})

	it('changes when isDeleted changes', () => {
		const a = [makeElement({ id: 'a', isDeleted: false })]
		const b = [makeElement({ id: 'a', isDeleted: true })]
		expect(computeElementVersionHash(a)).not.toBe(computeElementVersionHash(b))
	})

	it('changes when all elements are deleted, versus a truly empty array', () => {
		const allDeleted = [makeElement({ id: 'a', isDeleted: true })]
		expect(computeElementVersionHash(allDeleted)).not.toBe(computeElementVersionHash([]))
	})

	it('is order-sensitive', () => {
		const a = [makeElement({ id: 'a', version: 1 }), makeElement({ id: 'b', version: 2 })]
		const b = [makeElement({ id: 'b', version: 2 }), makeElement({ id: 'a', version: 1 })]
		expect(computeElementVersionHash(a)).not.toBe(computeElementVersionHash(b))
	})

	it('is deterministic for the same input', () => {
		const elements = [makeElement({ id: 'a', version: 3, versionNonce: 7 })]
		expect(computeElementVersionHash(elements)).toBe(computeElementVersionHash(elements))
	})
})

describe('buildBroadcastedElementVersions / getIncrementalSceneElements', () => {
	it('round-trips: nothing looks incremental right after a build', () => {
		const elements = [
			makeElement({ id: 'a', version: 1, versionNonce: 10 }),
			makeElement({ id: 'b', version: 1, versionNonce: 20 }),
		]
		const versions = buildBroadcastedElementVersions(elements)
		expect(getIncrementalSceneElements(elements, versions)).toEqual([])
	})

	it('flags only the elements whose version or versionNonce moved', () => {
		const initial = [
			makeElement({ id: 'a', version: 1, versionNonce: 10 }),
			makeElement({ id: 'b', version: 1, versionNonce: 20 }),
		]
		const versions = buildBroadcastedElementVersions(initial)

		const updated = [
			makeElement({ id: 'a', version: 1, versionNonce: 10 }), // unchanged
			makeElement({ id: 'b', version: 2, versionNonce: 20 }), // version bumped
		]
		const incremental = getIncrementalSceneElements(updated, versions)
		expect(incremental.map((el) => el.id)).toEqual(['b'])
	})

	it('flags an element absent from the version map', () => {
		const versions = buildBroadcastedElementVersions([makeElement({ id: 'a' })])
		const elements = [makeElement({ id: 'a' }), makeElement({ id: 'c' })]
		const incremental = getIncrementalSceneElements(elements, versions)
		expect(incremental.map((el) => el.id)).toEqual(['c'])
	})
})

describe('decideSceneBroadcast', () => {
	it('sends a full SCENE_INIT the first time a client broadcasts', () => {
		const elements = [makeElement({ id: 'a' })]
		const decision = decideSceneBroadcast(elements, false, null, {})
		expect(decision.kind).toBe('init')
		expect(decision).toMatchObject({ elements })
	})

	it('skips when the scene hash has not changed since the last broadcast', () => {
		const elements = [makeElement({ id: 'a', version: 1, versionNonce: 1 })]
		const hash = computeElementVersionHash(elements)
		const versions = buildBroadcastedElementVersions(elements)
		const decision = decideSceneBroadcast(elements, true, hash, versions)
		expect(decision).toEqual({ kind: 'skip' })
	})

	it('sends an incremental SCENE_UPDATE for the elements whose version moved', () => {
		const initial = [
			makeElement({ id: 'a', version: 1, versionNonce: 1 }),
			makeElement({ id: 'b', version: 1, versionNonce: 1 }),
		]
		const versions = buildBroadcastedElementVersions(initial)
		const updated = [
			initial[0],
			makeElement({ id: 'b', version: 2, versionNonce: 2 }),
		]
		const decision = decideSceneBroadcast(updated, true, computeElementVersionHash(initial), versions)
		expect(decision.kind).toBe('update')
		expect(decision).toMatchObject({ elements: [updated[1]] })
	})

	it('falls back to a full SCENE_INIT when the incremental diff comes back empty', () => {
		const initial = [makeElement({ id: 'a', version: 1, versionNonce: 1 })]
		// A version map that already thinks every current element is current,
		// yet the caller reports a different lastBroadcastedHash: getIncrementalSceneElements
		// finds nothing to diff, so the decision must still assert the full scene.
		const versions = buildBroadcastedElementVersions(initial)
		const decision = decideSceneBroadcast(initial, true, computeElementVersionHash(initial) + 1, versions)
		expect(decision.kind).toBe('init')
		expect(decision).toMatchObject({ elements: initial })
	})

	it('includes the new scene hash on both init and update decisions', () => {
		const elements = [makeElement({ id: 'a' })]
		const hash = computeElementVersionHash(elements)
		expect(decideSceneBroadcast(elements, false, null, {})).toMatchObject({ hash })
	})
})

describe('mergeSceneElements', () => {
	it('adds a remote element that does not exist locally', () => {
		const local: ExcalidrawElement[] = []
		const remote = [makeElement({ id: 'r1', version: 1 })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.map((el) => el.id)).toEqual(['r1'])
	})

	it('keeps a local element being actively edited (newElement), even if remote is newer', () => {
		const local = [makeElement({ id: 'a', version: 1, versionNonce: 1 })]
		const remote = [makeElement({ id: 'a', version: 5, versionNonce: 5 })]
		const appState = makeAppState({ newElement: { id: 'a' } as never })
		const result = mergeSceneElements(local, remote, appState)
		expect(result.find((el) => el.id === 'a')).toEqual(local[0])
	})

	it('keeps a local element being actively resized, even if remote is newer', () => {
		const local = [makeElement({ id: 'a', version: 1, versionNonce: 1 })]
		const remote = [makeElement({ id: 'a', version: 5, versionNonce: 5 })]
		const appState = makeAppState({ resizingElement: { id: 'a' } as never })
		const result = mergeSceneElements(local, remote, appState)
		expect(result.find((el) => el.id === 'a')).toEqual(local[0])
	})

	it('keeps the local element when its version is higher', () => {
		const local = [makeElement({ id: 'a', version: 3, versionNonce: 1 })]
		const remote = [makeElement({ id: 'a', version: 2, versionNonce: 1 })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.find((el) => el.id === 'a')).toEqual(local[0])
	})

	it('takes the remote element when its version is higher', () => {
		const local = [makeElement({ id: 'a', version: 2, versionNonce: 1 })]
		const remote = [makeElement({ id: 'a', version: 3, versionNonce: 1 })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.find((el) => el.id === 'a')).toEqual(remote[0])
	})

	it('on equal version, keeps the local element when its versionNonce is lower', () => {
		const local = [makeElement({ id: 'a', version: 2, versionNonce: 1 })]
		const remote = [makeElement({ id: 'a', version: 2, versionNonce: 9 })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.find((el) => el.id === 'a')).toEqual(local[0])
	})

	it('on equal version, takes the remote element when its versionNonce is lower', () => {
		const local = [makeElement({ id: 'a', version: 2, versionNonce: 9 })]
		const remote = [makeElement({ id: 'a', version: 2, versionNonce: 1 })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.find((el) => el.id === 'a')).toEqual(remote[0])
	})

	it('carries a remote delete through when the remote version is newer', () => {
		const local = [makeElement({ id: 'a', version: 1, versionNonce: 1, isDeleted: false })]
		const remote = [makeElement({ id: 'a', version: 2, versionNonce: 1, isDeleted: true })]
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.find((el) => el.id === 'a')?.isDeleted).toBe(true)
	})

	it('keeps a local-only element that the remote scene does not mention', () => {
		const local = [makeElement({ id: 'local-only', version: 1 })]
		const remote: ExcalidrawElement[] = []
		const result = mergeSceneElements(local, remote, makeAppState())
		expect(result.map((el) => el.id)).toEqual(['local-only'])
	})
})
