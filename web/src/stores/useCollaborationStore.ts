/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// This store has no presentation, voting, recording, or followed-user
// state: presenterId, isPresentationMode, isPresenting, presentationStartTime,
// autoFollowPresenter, votings, followedUserId, and their actions do not
// exist here.
//
// The `jwt_secret_mismatch` AuthErrorType stays even though this project
// seals the WOPI access token with a server secret rather than sharing a
// JWT secret between two services: a rotated server secret produces the
// same symptom (every session JWT this client holds fails validation), so
// the same circuit breaker applies.

import { create } from 'zustand'
import type { CollaborationSocket } from '../types/collaboration'

export type CollaborationConnectionStatus = 'online' | 'offline' | 'connecting' | 'reconnecting'

export type AuthErrorType = 'jwt_secret_mismatch' | 'token_expired' | 'unauthorized' | null

interface AuthErrorState {
	type: AuthErrorType
	message: string | null
	isPersistent: boolean // True when we've detected a persistent auth issue (likely a server secret rotation)
}

interface CollaborationStore {
	status: CollaborationConnectionStatus
	socket: CollaborationSocket | null
	isDedicatedSyncer: boolean // Is this client responsible for syncing to server/broadcasting?
	authError: AuthErrorState
	isInRoom: boolean // Whether the socket has joined the current room
	// True while the server reports this room in conflict (a foreign WOPI
	// lock, or a version drift, outside this client's own session). Set
	// from the initial GET /api/board/conflict, the conflict-state socket
	// event, and the poll fallback, all of which answer the same
	// {inConflict, saveStalled} shape.
	conflict: boolean
	// True while the server reports this room's saves to the WOPI host
	// keep failing, or the room has lost write access on every tracked
	// token: distinct from a conflict (an outside edit). Set from the same
	// three sources as `conflict`.
	saveStalled: boolean
	// True once this tab has committed to a full page reload (the
	// ConflictBanner Reload button, or the reload-required broadcast). Set
	// before the reload actually starts, since a beforeunload listener can
	// fire the instant window.location.reload() is called. useSync's
	// shouldSkipFinalServerSync and shouldSkipServerAPISync read this via
	// getState() and skip their PUT once it is set: the server already
	// dropped this room's retained scene for the reload, and a stale
	// cached scene re-posted from beforeunload would race the reloaded
	// page's own GET and overwrite the fresh content it is about to fetch.
	// Terminal once true: a reload it guards against is now certain to
	// happen, so resetStore (an unmount-time reset, not a completed reload)
	// must not clear it back to false and re-open that race.
	reloading: boolean

	// Actions
	setStatus: (status: CollaborationConnectionStatus) => void
	setSocket: (socket: CollaborationSocket | null) => void
	setDedicatedSyncer: (isSyncer: boolean) => void
	setIsInRoom: (inRoom: boolean) => void
	markTerminalAuthFailure: (errorType: AuthErrorType, message: string) => void
	clearAuthError: () => void
	setConflict: (inConflict: boolean) => void
	setSaveStalled: (saveStalled: boolean) => void
	setReloading: (reloading: boolean) => void
	resetStore: () => void
}

const initialAuthErrorState: AuthErrorState = {
	type: null,
	message: null,
	isPersistent: false,
}

const initialState: Omit<CollaborationStore, 'setStatus' | 'setSocket' | 'setDedicatedSyncer' | 'setIsInRoom' | 'markTerminalAuthFailure' | 'clearAuthError' | 'setConflict' | 'setSaveStalled' | 'setReloading' | 'resetStore'> = {
	status: 'offline',
	socket: null,
	isDedicatedSyncer: false,
	authError: initialAuthErrorState,
	isInRoom: false,
	conflict: false,
	saveStalled: false,
	reloading: false,
}

export const useCollaborationStore = create<CollaborationStore>()((set) => ({
	...initialState,

	setStatus: (status) => set((state) => (state.status === status ? {} : { status })),
	setSocket: (socket) => set({ socket }),
	setDedicatedSyncer: (isSyncer) => set({ isDedicatedSyncer: isSyncer }),
	setIsInRoom: (inRoom) => set({ isInRoom: inRoom }),

	// For an error kind with no retry path (e.g. jwt_secret_mismatch: the
	// session JWT is minted once, at launch), the first failure is already
	// terminal.
	markTerminalAuthFailure: (errorType, message) => set({
		authError: {
			type: errorType,
			message,
			isPersistent: true,
		},
	}),

	clearAuthError: () => set({ authError: initialAuthErrorState }),

	setConflict: (inConflict) => set((state) => (state.conflict === inConflict ? {} : { conflict: inConflict })),
	setSaveStalled: (saveStalled) => set((state) => (state.saveStalled === saveStalled ? {} : { saveStalled })),
	setReloading: (reloading) => set((state) => (state.reloading === reloading ? {} : { reloading })),

	// reloading is deliberately not in initialState's spread here: see that
	// field's comment for why a reset must not clear it.
	resetStore: () => set((state) => ({ ...initialState, reloading: state.reloading })),
}))
