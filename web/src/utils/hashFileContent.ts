// SPDX-License-Identifier: AGPL-3.0-or-later

// useSync uses this to dedupe IMAGE_ADD broadcasts: it skips re-sending a
// file whose content did not change since the last websocket sync.

/**
 * Builds a cheap, non-cryptographic fingerprint of a data URL: its length
 * plus its first and last 20 characters. Good enough to detect "this file
 * changed" without hashing the whole payload on every sync tick.
 */
export function hashFileContent(content: string): string {
	if (!content) return ''
	const len = content.length
	const start = content.substring(0, 20)
	const end = content.substring(Math.max(0, len - 20))
	return `${len}:${start}:${end}`
}
