// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { db } from './db'

describe('WhiteboardDatabase schema', () => {
	it('declares exactly one schema version', () => {
		expect(db.tables.map(t => t.name)).toEqual(['whiteboards'])
	})

	it('keys the whiteboards store by a plain id, not an auto-increment', () => {
		const primKey = db.table('whiteboards').schema.primKey
		expect(primKey.name).toBe('id')
		expect(primKey.auto).toBe(false)
	})

	it('indexes savedAt', () => {
		const indexNames = db.table('whiteboards').schema.indexes.map(i => i.name)
		expect(indexNames).toEqual(['savedAt'])
	})
})

// happy-dom (this project's vitest environment) ships no IndexedDB
// implementation at all — only Dexie's in-memory schema metadata (exercised
// by the tests above) works here; an actual get()/put() round trip needs a
// real browser or a fake-IndexedDB polyfill this project does not depend on.
// So this test works at the unit level instead: mocking Dexie.Dexie's own
// transaction() lets the test observe, without ever opening a real
// database, that put() (a) wraps its read and its write in one 'rw'
// transaction against the whiteboards table (a real 'rw' transaction
// serializes against any other transaction touching that table, so a
// concurrent put() for the same or a different fileId can no longer
// interleave its own get() between another call's get() and put()), and
// (b) still merges the existing row's fields correctly inside that
// transaction.
describe('WhiteboardDatabase.put race safety', () => {
	it('wraps its get-then-put pair in a single rw transaction against the whiteboards table', async () => {
		const transactionSpy = vi.spyOn(db, 'transaction')
			.mockImplementation((...args: unknown[]) => {
				const scope = args[args.length - 1] as () => Promise<unknown>
				return scope() as never
			})
		const getSpy = vi.spyOn(db.whiteboards, 'get').mockResolvedValue({
			id: 'f1',
			elements: [],
			files: {},
			hasPendingLocalChanges: true,
			lastSyncedHash: 42,
		})
		const putSpy = vi.spyOn(db.whiteboards, 'put').mockResolvedValue('f1')

		try {
			await db.put('f1', [], {}, undefined, {})

			expect(transactionSpy).toHaveBeenCalledTimes(1)
			expect(transactionSpy.mock.calls[0][0]).toBe('rw')
			expect(transactionSpy.mock.calls[0][1]).toBe(db.whiteboards)

			// Called from inside the transaction's scope callback, so the
			// get()-then-put() ordering is preserved and both calls
			// happened inside the same transaction() invocation.
			expect(getSpy).toHaveBeenCalledWith('f1')
			expect(putSpy).toHaveBeenCalledWith(expect.objectContaining({
				id: 'f1',
				// Falls back to the existing row's values (fetched inside
				// the same transaction) when the caller passes no options.
				hasPendingLocalChanges: true,
				lastSyncedHash: 42,
			}))
		} finally {
			transactionSpy.mockRestore()
			getSpy.mockRestore()
			putSpy.mockRestore()
		}
	})
})

describe('WhiteboardDatabase.clearPendingLocalChanges', () => {
	it('patches hasPendingLocalChanges to false without a full get-then-put', async () => {
		const updateSpy = vi.spyOn(db.whiteboards, 'update').mockResolvedValue(1)

		try {
			await db.clearPendingLocalChanges('f1')

			expect(updateSpy).toHaveBeenCalledWith('f1', { hasPendingLocalChanges: false })
		} finally {
			updateSpy.mockRestore()
		}
	})
})
