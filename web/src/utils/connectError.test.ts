// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it } from 'vitest'
import { classifyConnectError } from './connectError'

describe('classifyConnectError', () => {
	it('classifies the relay wrong-replica rejection', () => {
		expect(classifyConnectError('wrong replica')).toBe('wrong-replica')
	})

	it('classifies an authentication rejection', () => {
		expect(classifyConnectError('Authentication error')).toBe('auth-failure')
	})

	it('never classifies a wrong-replica message as an auth failure', () => {
		expect(classifyConnectError('wrong replica')).not.toBe('auth-failure')
	})

	it('falls back to other for an unrelated message', () => {
		expect(classifyConnectError('transport close')).toBe('other')
	})
})
