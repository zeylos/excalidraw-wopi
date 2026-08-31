// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import {
	createSceneDedupTracker,
	isDuplicateScenePayload,
	markScenePayloadApplied,
	resetSceneDedup,
} from './sceneDedup'

describe('sceneDedup', () => {
	it('never reports a duplicate before anything has been applied', () => {
		const tracker = createSceneDedupTracker()
		expect(isDuplicateScenePayload(tracker, '[]')).toBe(false)
	})

	it('reports a duplicate once a payload has actually been marked applied', () => {
		const tracker = createSceneDedupTracker()
		markScenePayloadApplied(tracker, '[{"id":"a"}]')
		expect(isDuplicateScenePayload(tracker, '[{"id":"a"}]')).toBe(true)
		expect(isDuplicateScenePayload(tracker, '[{"id":"b"}]')).toBe(false)
	})

	it('does not treat a queued-but-not-applied payload as a duplicate', () => {
		const tracker = createSceneDedupTracker()
		// Simulates queueSceneUpdate deferring to pendingSceneUpdateRef because
		// excalidrawAPI was not ready yet: no markScenePayloadApplied call.
		expect(isDuplicateScenePayload(tracker, '[{"id":"a"}]')).toBe(false)

		// A fileId change discards the pending queue.
		resetSceneDedup(tracker)

		// The same scene arrives again later; it must still be treated as new.
		expect(isDuplicateScenePayload(tracker, '[{"id":"a"}]')).toBe(false)
	})

	it('clears the fingerprint on reset even after a real apply', () => {
		const tracker = createSceneDedupTracker()
		markScenePayloadApplied(tracker, '[{"id":"a"}]')
		resetSceneDedup(tracker)
		expect(isDuplicateScenePayload(tracker, '[{"id":"a"}]')).toBe(false)
	})
})
