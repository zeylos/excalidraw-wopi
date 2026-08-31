// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { estimateDataUrlBytes, exceedsImageSizeLimit, shouldRejectRemoteImage } from './imageSizeLimit'

describe('estimateDataUrlBytes', () => {
	it('returns 0 for a URL with no base64 payload', () => {
		expect(estimateDataUrlBytes('not-a-data-url')).toBe(0)
	})

	it('estimates the decoded size of an unpadded base64 payload', () => {
		// 'AAAA' decodes to 3 bytes.
		expect(estimateDataUrlBytes('data:image/png;base64,AAAA')).toBe(3)
	})

	it('accounts for one padding character', () => {
		// 'AAA=' decodes to 2 bytes.
		expect(estimateDataUrlBytes('data:image/png;base64,AAA=')).toBe(2)
	})

	it('accounts for two padding characters', () => {
		// 'AA==' decodes to 1 byte.
		expect(estimateDataUrlBytes('data:image/png;base64,AA==')).toBe(1)
	})
})

describe('exceedsImageSizeLimit', () => {
	it('returns false when the decoded size is under the limit', () => {
		expect(exceedsImageSizeLimit('data:image/png;base64,AAAA', 3)).toBe(false)
	})

	it('returns false when the decoded size equals the limit', () => {
		expect(exceedsImageSizeLimit('data:image/png;base64,AAAA', 3)).toBe(false)
	})

	it('returns true when the decoded size exceeds the limit', () => {
		expect(exceedsImageSizeLimit('data:image/png;base64,AAAA', 2)).toBe(true)
	})

	it('honors a limit sourced from a configured value distinct from the 10 MB Go default', () => {
		const fiveMegabytes = 5 * 1024 * 1024
		const base64OverFiveMb = 'A'.repeat(Math.ceil((fiveMegabytes + 1) * 4 / 3))
		expect(exceedsImageSizeLimit(`data:image/png;base64,${base64OverFiveMb}`, fiveMegabytes)).toBe(true)
	})
})

// useCollaboration's receive-side gate on an incoming IMAGE_ADD payload, so
// a malicious peer cannot push an oversized image and
// OOM other tabs in the room. useCollaboration.ts cannot itself be imported
// under vitest (importing '@excalidraw/excalidraw' pulls in an open-color
// JSON import this project's Vite/Node setup rejects outside a real build,
// per bootstrap.test.ts's comment), so this pure decision function is the
// unit under test for that gate.
describe('shouldRejectRemoteImage', () => {
	it('accepts an image at or under the limit', () => {
		expect(shouldRejectRemoteImage({ dataURL: 'data:image/png;base64,AAAA' }, 3)).toBe(false)
	})

	it('rejects an image over the limit', () => {
		expect(shouldRejectRemoteImage({ dataURL: 'data:image/png;base64,AAAA' }, 2)).toBe(true)
	})

	it('accepts a file with no dataURL (nothing to measure)', () => {
		expect(shouldRejectRemoteImage({}, 2)).toBe(false)
	})
})
