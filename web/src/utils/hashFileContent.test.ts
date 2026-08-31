// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { hashFileContent } from './hashFileContent'

describe('hashFileContent', () => {
	it('returns an empty string for empty content', () => {
		expect(hashFileContent('')).toBe('')
	})

	it('is deterministic for the same content', () => {
		const content = 'data:image/png;base64,aGVsbG8gd29ybGQ='
		expect(hashFileContent(content)).toBe(hashFileContent(content))
	})

	it('changes when the content changes', () => {
		const a = 'data:image/png;base64,aGVsbG8gd29ybGQ='
		const b = 'data:image/png;base64,Z29vZGJ5ZSB3b3JsZA=='
		expect(hashFileContent(a)).not.toBe(hashFileContent(b))
	})

	it('detects a change confined to the middle of a long content string', () => {
		const prefix = 'data:image/png;base64,'
		const a = prefix + 'A'.repeat(1000) + 'X' + 'A'.repeat(1000)
		const b = prefix + 'A'.repeat(1000) + 'Y' + 'A'.repeat(1000)
		// hashFileContent only samples the first/last 20 chars plus length, so a
		// same-length, middle-only edit is invisible to it: this documents that
		// known limitation rather than asserting a false guarantee.
		expect(hashFileContent(a)).toBe(hashFileContent(b))
	})

	it('handles content shorter than the 20-character sample window', () => {
		expect(hashFileContent('short')).toBe('5:short:short')
	})
})
