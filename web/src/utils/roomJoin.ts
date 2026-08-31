// SPDX-License-Identifier: AGPL-3.0-or-later

// Factored out of useCollaboration.ts's handleInitRoom, which compares
// this against a joinedRoomRef. useCollaboration still owns the ref and the
// debounced socket.emit('join-room', ...) call; this only decides whether
// that call is due, so the decision is testable without a socket.

/**
 * Reports whether the client must (re-)send join-room for roomId: true
 * unless it already holds a join tracked for the very same room. The
 * `init-room` handler always calls this with joinedRoomId reset to null
 * first (a fresh connection or a reconnect never trusts a stale room join).
 */
export function shouldJoinRoom(joinedRoomId: string | null, roomId: string): boolean {
	return joinedRoomId !== roomId
}

/**
 * Reports whether a join-room emit that received no room-user-change
 * confirmation within the timeout window is worth retrying: bounded by
 * maxAttempts, so a room the relay never confirms does not retry forever.
 * Factored out of useCollaboration.ts's attemptJoinRoom, so the retry
 * bound is testable without a socket.
 */
export function shouldRetryJoin(attemptCount: number, maxAttempts: number): boolean {
	return attemptCount < maxAttempts
}
