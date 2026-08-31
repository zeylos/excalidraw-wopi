/**
 * SPDX-FileCopyrightText: 2020 Excalidraw, 2024 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: MIT
 */

// This component does not implement: recording (useRecording,
// RecordingOverlay), presentation (usePresentation, PresentationOverlay),
// timer (useTimer, TimerOverlay), voting (useVoting, VotingSidebar), comments
// (useComment, CommentSidebar), the assistant (useAssistant), the smart
// picker (useSmartPicker), the emoji picker (useEmojiPicker), table
// insertion (useTableInsertion), element-creator tracking
// (useElementCreatorTracking, CreatorDisplay), followed-user
// (useFollowedUser), version preview (useVersionPreview,
// VersionPreviewBanner, the files_versions:restore:requested subscription),
// the library and canvas-template save dialogs (useLibrary,
// useLibraryCatalog, useCanvasTemplate, SaveScopedDialog,
// onLibrarySaveAs/onSaveAsCanvasTemplate/libraryMenuTitle — props
// that do not exist in stock 0.18), external-library disabling
// (useDisableExternalLibraries), the context-menu filter
// (useContextMenuFilter), the sidebar download hook, the mobile bridge
// (callMobileMessage), and the custom Embeddable renderer. `beforeElementCreated`
// and the onDuplicate wiring built on it are also absent: this project never
// stamps `customData.creator`, so there is nothing left for them to do.
//
// Also absent: useThemeHandling and useLangStore (replaced below by a small
// local system-theme listener and a static langCode default), and
// useHandleLibrary (no library feature here).
//
// Kept: the core Excalidraw render (initialData from the config store's
// promise, the excalidrawAPI callback into useExcalidrawStore, onChange/
// onPointerUpdate from useSync, viewModeEnabled from useReadOnlyState,
// UIOptions, langCode, theme), the useCollaboration and useBoardDataManager
// lifecycles, and a small stock MainMenu (see the inline comment below).

import { memo, useEffect, useMemo, useState } from 'react'
import { Excalidraw, MainMenu } from '@excalidraw/excalidraw'
import '@excalidraw/excalidraw/index.css'
import './styles/overrides/_excalidraw.scss'
import { useShallow } from 'zustand/react/shallow'
import { useExcalidrawStore } from './stores/useExcalidrawStore'
import { useWhiteboardConfigStore } from './stores/useWhiteboardConfigStore'
import { useSyncStore } from './stores/useSyncStore'
import { useCollaboration } from './hooks/useCollaboration'
import { useBoardDataManager } from './hooks/useBoardDataManager'
import { useReadOnlyState } from './hooks/useReadOnlyState'
import { useSync } from './hooks/useSync'
import { NetworkStatusIndicator } from './components/NetworkStatusIndicator'
import { AuthErrorNotification } from './components/AuthErrorNotification'
import { ConflictBanner } from './components/ConflictBanner'
import { useSessionStore } from './stores/useSessionStore'
import { useCollaborationStore } from './stores/useCollaborationStore'
import { fetchConflictState } from './utils/conflictApi'
import logger from './utils/logger'
import type { Theme } from '@excalidraw/excalidraw/element/types'

const MemoizedExcalidraw = memo(Excalidraw)
const MemoizedNetworkStatusIndicator = memo(NetworkStatusIndicator)
const MemoizedAuthErrorNotification = memo(AuthErrorNotification)
const MemoizedConflictBanner = memo(ConflictBanner)

// The poll fallback for the conflict-state push. The live path is the
// socket event (useCollaboration.ts registers it); this only covers the
// gap while the socket is down or briefly reconnecting, so a long interval
// is fine — the banner is not time-critical the way presence or scene sync
// are.
const CONFLICT_POLL_INTERVAL_MS = 15000

/**
 * Fetches the conflict state once and, while fileId stays the same, on a
 * fixed interval, keeping useCollaborationStore's conflict flag current
 * even if the conflict-state socket event never arrives (a down or briefly
 * reconnecting socket). Only polls while the socket is not online: once
 * connected, the socket pushes conflict-state live, so a 15s poll running
 * alongside it would just be redundant traffic.
 */
function useConflictPolling(apiBase: string, fileId: string) {
	const status = useCollaborationStore(state => state.status)

	useEffect(() => {
		if (!fileId || status === 'online') {
			return
		}

		let cancelled = false
		const poll = async () => {
			const token = useSessionStore.getState().getToken()
			if (!token) {
				return
			}
			const state = await fetchConflictState(apiBase, token, fileId)
			if (!cancelled && state !== null) {
				useCollaborationStore.getState().setConflict(state.inConflict)
				useCollaborationStore.getState().setSaveStalled(state.saveStalled)
			}
		}

		poll()
		const interval = setInterval(poll, CONFLICT_POLL_INTERVAL_MS)
		return () => {
			cancelled = true
			clearInterval(interval)
		}
	}, [apiBase, fileId, status])
}

// This project has no per-user theme override, so it reads only the
// system color-scheme preference.
function useSystemTheme(): Theme {
	const getTheme = (): Theme => (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
	const [theme, setTheme] = useState<Theme>(getTheme)

	useEffect(() => {
		const mq = window.matchMedia('(prefers-color-scheme: dark)')
		const listener = () => setTheme(getTheme())
		mq.addEventListener('change', listener)
		return () => mq.removeEventListener('change', listener)
	}, [])

	return theme
}

export interface AppProps {
	apiBase: string
	socketPath: string
	maxImageBytes: number
}

export default function App({ apiBase, socketPath, maxImageBytes }: AppProps) {
	const fileId = useWhiteboardConfigStore(state => state.fileId)
	const { fileName, initialDataPromise, resetStore } = useWhiteboardConfigStore(useShallow(state => ({
		fileName: state.fileName,
		initialDataPromise: state.initialDataPromise,
		resetStore: state.resetStore,
	})))

	const { setExcalidrawAPI, resetExcalidrawAPI } = useExcalidrawStore(useShallow(state => ({
		setExcalidrawAPI: state.setExcalidrawAPI,
		resetExcalidrawAPI: state.resetExcalidrawAPI,
	})))

	const terminateWorker = useSyncStore(state => state.terminateWorker)

	// Both hooks connect/load reactively off the fileId already set in the
	// config store (main.tsx sets it before this component ever mounts), and
	// they already tear themselves down on unmount; App only needs to run
	// them and read back what it renders with.
	useCollaboration({ socketPath, maxImageBytes })
	const { isLoading, saveOnUnmount } = useBoardDataManager({ apiBase })
	const { isReadOnly } = useReadOnlyState()
	const { onChange, onPointerUpdate } = useSync({ apiBase, maxImageBytes })
	const theme = useSystemTheme()
	useConflictPolling(apiBase, fileId)

	const fileNameWithoutExtension = useMemo(() => {
		const withoutExtension = fileName.split('.').slice(0, -1).join('.')
		return withoutExtension || fileName
	}, [fileName])

	const langCode = useMemo(() => document.documentElement.lang || 'en', [])

	useEffect(() => {
		return () => {
			// terminateWorker only after the unmount save settles: useSync's own
			// worker-init effect does not terminate it on cleanup, so this save
			// always finds a live worker.
			saveOnUnmount(() => terminateWorker())
			resetStore()
			resetExcalidrawAPI()
		}
	}, [saveOnUnmount, resetStore, resetExcalidrawAPI, terminateWorker])

	if (!fileId) {
		logger.warn('[App] No fileId in the whiteboard config store.')
		return <div className="App App-error">This whiteboard has no file ID.</div>
	}

	if (isLoading) {
		return (
			<div className="App" style={{ display: 'flex', flexDirection: 'column' }}>
				<div className="App-loading" style={{ flex: 1, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
					Loading whiteboard...
				</div>
			</div>
		)
	}

	return (
		<div className="App" style={{ display: 'flex', flexDirection: 'column' }}>
			<div className="excalidraw-wrapper" style={{ flex: 1, height: '100%', position: 'relative' }}>
				<MemoizedNetworkStatusIndicator />
				<MemoizedAuthErrorNotification />
				<MemoizedConflictBanner apiBase={apiBase} />
				<MemoizedExcalidraw
					excalidrawAPI={setExcalidrawAPI}
					initialData={initialDataPromise}
					onChange={onChange}
					onPointerUpdate={onPointerUpdate}
					viewModeEnabled={isReadOnly}
					theme={theme}
					name={fileNameWithoutExtension}
					langCode={langCode}
					UIOptions={{ canvasActions: { loadScene: false } }}
				>
					{/*
					 * This project has no use for share, save-to-server, smart picker,
					 * or library entries, and no menu entry belongs to an absent
					 * feature (recording, presentation, timer, voting, creator
					 * attribution, grid toggle), so nothing custom is left. This
					 * renders the stock MainMenu inline instead of keeping a
					 * near-empty wrapper component, with only the default
					 * SaveAsImage, Export, ToggleTheme, and Help items.
					 */}
					<MainMenu>
						<MainMenu.DefaultItems.SaveAsImage />
						<MainMenu.DefaultItems.Export />
						<MainMenu.DefaultItems.ToggleTheme />
						<MainMenu.DefaultItems.Help />
					</MainMenu>
				</MemoizedExcalidraw>
			</div>
		</div>
	)
}
