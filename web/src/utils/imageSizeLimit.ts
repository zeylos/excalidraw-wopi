// SPDX-License-Identifier: AGPL-3.0-or-later

// Client-side half of the env-configurable image size limit. The Go side is
// EXCALIDRAW_WOPI_MAX_IMAGE_BYTES (internal/config), injected into
// AppConfig.maxImageBytes by internal/launch and threaded down through
// App's props to useSync (send side) and useCollaboration (receive side):
// both call exceedsImageSizeLimit against the same operator-configured
// value, so there is exactly one limit in effect.

/**
 * Estimates the decoded byte size of a `data:...;base64,...` URL from its
 * base64 payload length, without decoding it. Returns 0 for a URL with no
 * base64 payload (e.g. a plain data URL with no comma).
 */
export function estimateDataUrlBytes(dataUrl: string): number {
	const commaIndex = dataUrl.indexOf(',')
	if (commaIndex === -1) {
		return 0
	}

	const base64 = dataUrl.slice(commaIndex + 1)
	const padding = base64.endsWith('==') ? 2 : base64.endsWith('=') ? 1 : 0
	return Math.floor((base64.length * 3) / 4) - padding
}

/**
 * Reports whether dataUrl's estimated decoded size exceeds maxImageBytes.
 * Both useSync (send side) and useCollaboration (receive side) call this
 * against the same AppConfig.maxImageBytes value.
 */
export function exceedsImageSizeLimit(dataUrl: string, maxImageBytes: number): boolean {
	return estimateDataUrlBytes(dataUrl) > maxImageBytes
}

/**
 * Reports whether useCollaboration's handleRemoteImageAdd must reject an
 * incoming IMAGE_ADD payload: a peer, malicious or buggy, could otherwise
 * push an image past maxImageBytes and OOM every other tab in the room. A
 * file with no dataURL cannot be measured, so it passes through unrejected.
 */
export function shouldRejectRemoteImage(file: { dataURL?: string }, maxImageBytes: number): boolean {
	return Boolean(file.dataURL) && exceedsImageSizeLimit(file.dataURL as string, maxImageBytes)
}
