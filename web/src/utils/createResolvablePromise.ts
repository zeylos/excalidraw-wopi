/**
 * SPDX-FileCopyrightText: 2020 Excalidraw
 * SPDX-License-Identifier: MIT
 */

/* eslint-disable @typescript-eslint/no-explicit-any */

// Stock 0.18 exports ResolvablePromise as a named type, not an ambient global.
import type { ResolvablePromise } from '@excalidraw/excalidraw/utils'

export const createResolvablePromise = () => {
	let resolve!: any
	let reject!: any
	const promise = new Promise((_resolve, _reject) => {
		resolve = _resolve
		reject = _reject
	})
	;(promise as any).resolve = resolve
	;(promise as any).reject = reject
	return promise as ResolvablePromise<any>
}
