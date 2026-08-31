// SPDX-License-Identifier: AGPL-3.0-or-later

// The session JWT is minted once, at launch, by the Go server, and handed
// to the browser in the AppConfig blob; it is not refreshed. The server
// itself warns, flushes, and disconnects the session before the token
// expires, and a relaunch mints a fresh one. So this store only holds what
// bootstrap gave it and exposes it.

import { create } from 'zustand'
import type { AppConfig } from '../config'

interface SessionState {
	token: string | null
	userId: string | null
	userName: string | null
	canWrite: boolean
	isReadOnly: boolean

	// Set once at bootstrap from the AppConfig blob; see config.ts loadConfig().
	setSession: (config: Pick<AppConfig, 'sessionToken' | 'userId' | 'userName' | 'canWrite'>) => void
	getToken: () => string | null
}

export const useSessionStore = create<SessionState>((set, get) => ({
	token: null,
	userId: null,
	userName: null,
	canWrite: false,
	isReadOnly: false,

	setSession: (config) => set({
		token: config.sessionToken,
		userId: config.userId,
		userName: config.userName,
		canWrite: config.canWrite,
		isReadOnly: !config.canWrite,
	}),

	getToken: () => get().token,
}))
