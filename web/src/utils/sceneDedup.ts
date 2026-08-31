// SPDX-License-Identifier: AGPL-3.0-or-later

// Factored out of useCollaboration.ts's lastElementsStringRef dedup for
// excalidraw-wopi, so the "was this payload already applied" decision is
// testable without a socket or an Excalidraw instance. useCollaboration
// still owns the tracker instance (one useRef per hook) and calls these
// around SCENE_INIT/SCENE_UPDATE handling.

export interface SceneDedupTracker {
	lastAppliedElementsString: string | null
}

export function createSceneDedupTracker(): SceneDedupTracker {
	return { lastAppliedElementsString: null }
}

/** True when elementsString matches the last payload actually applied to the scene. */
export function isDuplicateScenePayload(tracker: SceneDedupTracker, elementsString: string): boolean {
	return tracker.lastAppliedElementsString === elementsString
}

/**
 * Marks elementsString as applied. Call only once the payload actually
 * reaches the scene (reconcileAndApplyRemoteElements), never at decode or
 * queue time: marking it earlier lets a payload that is later discarded
 * (e.g. a fileId change clearing the pending queue) permanently dedupe an
 * identical future broadcast — including the 20s healing rebroadcast —
 * without it ever having been applied.
 */
export function markScenePayloadApplied(tracker: SceneDedupTracker, elementsString: string): void {
	tracker.lastAppliedElementsString = elementsString
}

/**
 * Clears the dedup fingerprint. Call wherever a queued-but-not-yet-applied
 * payload is discarded (a fileId change) or the active socket changes: a
 * fingerprint from a payload that never landed, or from a previous session,
 * must not suppress a genuinely new one.
 */
export function resetSceneDedup(tracker: SceneDedupTracker): void {
	tracker.lastAppliedElementsString = null
}
