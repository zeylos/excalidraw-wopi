// SPDX-License-Identifier: AGPL-3.0-or-later

// Multi-replica support: a server-side router reverse-proxies /api/board*
// and /socket.io/ requests to the replica that owns the file, keyed on this
// query parameter. The router reads it because the real fileId lives only
// inside the sealed session token, which a router cannot open cheaply. The
// server re-checks ownership against the token on every request, so a wrong
// or missing parameter is never a security issue, only a routing one — the
// server ignores it outright in single-replica mode. Every request to those
// endpoints must still carry it, so this call is unconditional.

/**
 * Appends `room=<fileId>` to url for the router described above. fileId is
 * server-derived but treated as untrusted data here, so it is percent-encoded.
 */
export function withRoomParam(url: string, fileId: string): string {
	const separator = url.includes('?') ? '&' : '?'
	return `${url}${separator}room=${encodeURIComponent(fileId)}`
}
