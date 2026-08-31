// SPDX-License-Identifier: AGPL-3.0-or-later

// Factored out of useCollaboration.ts's connect_error handler so the branch
// choice is testable without a socket. The two messages come from different
// layers and can never both match: "wrong replica" is
// internal/relay/relay.go's wrongReplicaMessage, a namespace-middleware
// rejection issued per handshake when the session's file belongs to another
// replica; "Authentication error" is socket.io-client's own wording for a
// rejected auth middleware call.

export type ConnectErrorClass = 'wrong-replica' | 'auth-failure' | 'other'

/**
 * Classifies a socket.io connect_error message so the caller can pick the
 * right recovery: a wrong-replica rejection is transient and worth a fresh
 * connection attempt, an auth failure is terminal (this project has no
 * token refresh path), and anything else falls back to the generic
 * offline state.
 */
export function classifyConnectError(message: string): ConnectErrorClass {
	if (message.includes('wrong replica')) {
		return 'wrong-replica'
	}
	if (message.includes('Authentication error')) {
		return 'auth-failure'
	}
	return 'other'
}
