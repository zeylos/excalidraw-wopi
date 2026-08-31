// SPDX-License-Identifier: AGPL-3.0-or-later

// This is the client half of the conflict path. The server pauses saves and
// reports {inConflict: true} (internal/room's Manager, pushed live over
// internal/relay's conflict-state event and polled as a fallback via GET
// /api/board/conflict, both wired in App.tsx) once a foreign WOPI lock or a
// version drift shows the file changed on the WOPI host outside this session.
// This banner surfaces that state and, for a writer, offers two actions:
// Overwrite (push this session's scene, discarding the outside change) and
// Reload (discard this session's unsaved state and pick up the host's current
// content). A read-only viewer sees the notice with no actions: only a
// writer's choice resolves a room's conflict (internal/boardapi's 403 on that
// endpoint for a read-only session enforces the same rule server-side).
//
// The same conflict-state payload also carries saveStalled: a dirty room
// whose saves to the host keep failing, or one that has lost write access on
// every tracked token (e.g. the file was deleted or
// write ability was revoked on the host). Both are separate conditions, not
// conflicts: a host, network, or permission fault, not an outside edit.
// There is nothing to resolve here. A writer's reload mints a fresh WOPI
// token that can restore saving once host access returns (for example
// after a trash restore). A read-only viewer's token stays canWrite:false
// after a reload, so saving never resumes. The banner shows the Reload
// button to writers only.
//
// Styled like AuthErrorNotification.tsx: plain inline styles, no icon
// library, since neither is a dependency of this project.

import { useCallback, useState, type CSSProperties } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import { useSessionStore } from '../stores/useSessionStore'
import { useWhiteboardConfigStore } from '../stores/useWhiteboardConfigStore'
import { resolveConflict } from '../utils/conflictApi'
import { db } from '../database/db'
import logger from '../utils/logger'

/**
 * Pure render-decision logic, factored out of the component so it is
 * testable without mounting React (this project has no @testing-library/
 * react dependency; AuthErrorNotification.tsx's getAuthErrorConfig and
 * NetworkStatusIndicator.tsx's getStatusConfig set the same precedent).
 */
export function getConflictBannerState(
	conflict: boolean,
	saveStalled: boolean,
	isReadOnly: boolean,
): { visible: boolean; showActions: boolean; showStalledReload: boolean } {
	return {
		visible: conflict || saveStalled,
		showActions: conflict && !isReadOnly,
		showStalledReload: saveStalled && !conflict && !isReadOnly,
	}
}

const containerStyle: CSSProperties = {
	position: 'fixed',
	top: 16,
	left: '50%',
	transform: 'translateX(-50%)',
	zIndex: 10000,
	maxWidth: 460,
	minWidth: 320,
	padding: 16,
	borderRadius: 8,
	boxShadow: '0 4px 12px rgba(0, 0, 0, 0.15)',
	border: '1px solid #92400e',
	color: '#92400e',
	backgroundColor: '#fffbeb',
}

const buttonStyle: CSSProperties = {
	padding: '6px 14px',
	borderRadius: 4,
	border: '1px solid currentColor',
	background: 'none',
	color: 'inherit',
	fontSize: 13,
	fontWeight: 600,
	cursor: 'pointer',
}

const rowStyle: CSSProperties = {
	display: 'flex',
	gap: 8,
}

export interface ConflictBannerProps {
	apiBase: string
}

export function ConflictBanner({ apiBase }: ConflictBannerProps) {
	const { conflict, saveStalled, setConflict } = useCollaborationStore(
		useShallow(state => ({
			conflict: state.conflict,
			saveStalled: state.saveStalled,
			setConflict: state.setConflict,
		})),
	)
	const isReadOnly = useSessionStore(state => state.isReadOnly)
	const fileId = useWhiteboardConfigStore(state => state.fileId)
	const [isResolving, setIsResolving] = useState(false)

	const { visible, showActions, showStalledReload } = getConflictBannerState(conflict, saveStalled, isReadOnly)

	// A full page reload is the deliberate choice here, not a fetch-and-
	// updateScene swap: after a resolve(overwrite:false), the room's
	// retained scene is gone server-side (internal/room's ResolveConflict
	// drops it) and the next load must run the exact same path a fresh
	// launch already uses (useBoardDataManager's loadBoard, through
	// fetchWhiteboardSnapshot) to pick up the host's content, re-derive the
	// IndexedDB pending-changes flag, and rejoin the collaboration room
	// cleanly. Reusing that path in place would mean re-deriving several
	// pieces of hook-local state (useCollaboration's scene dedup
	// fingerprint, useSync's broadcast-version tracking, the sync worker's
	// own state) that only make sense mid-session; a reload restarts all of
	// them consistently, at the cost of the brief flash a real navigation
	// causes. Given this is an out-of-band host-side edit, not a frequent
	// interaction, that trade favors correctness over smoothness.
	const handleReload = useCallback(async () => {
		if (isResolving) return
		setIsResolving(true)
		// Set before the resolve call, not in the finally block: see
		// useCollaborationStore's `reloading` field for why.
		useCollaborationStore.getState().setReloading(true)
		try {
			const token = useSessionStore.getState().getToken()
			if (token) {
				await resolveConflict(apiBase, token, fileId, false)
			}
		} finally {
			// This is the user's own choice to discard this session's edits, so
			// the reload must not let them come back through the IndexedDB
			// reconcile on the next load (see db.clearPendingLocalChanges).
			// Never let a storage error block the reload itself.
			try {
				await db.clearPendingLocalChanges(fileId)
			} catch (error) {
				logger.error('[ConflictBanner] Failed to clear pending local changes before reload:', error)
			}
			window.location.reload()
		}
	}, [apiBase, isResolving, fileId])

	const handleOverwrite = useCallback(async () => {
		if (isResolving) return
		setIsResolving(true)
		try {
			const token = useSessionStore.getState().getToken()
			if (!token) return
			const ok = await resolveConflict(apiBase, token, fileId, true)
			if (ok) {
				// The server also pushes conflict-state:false back over the
				// socket (internal/room's OnConflictChange), but clearing it
				// here too means the banner drops immediately even if that
				// broadcast is delayed or the socket is briefly down.
				setConflict(false)
			}
		} catch (error) {
			logger.error('[ConflictBanner] Overwrite failed:', error)
		} finally {
			setIsResolving(false)
		}
	}, [apiBase, isResolving, setConflict, fileId])

	if (!visible) {
		return null
	}

	return (
		<div style={containerStyle} role="alert">
			<div style={{ fontWeight: 600, fontSize: 14, marginBottom: 4 }}>
				{conflict ? 'This board changed outside this session' : 'Saving is having trouble'}
			</div>
			<div style={{ fontSize: 13, lineHeight: 1.4, marginBottom: showActions || showStalledReload ? 12 : 0 }}>
				{conflict
					? (showActions
						? 'Someone edited this file outside this session. Overwrite to keep your changes, or reload to see the latest version.'
						: 'Someone edited this file outside this session. Saving is paused until a writer resolves this.')
					: (isReadOnly
						? "The server cannot save this board's changes right now. The unsaved edits stay in the session."
						: 'The server cannot save your changes right now. Your edits stay in this session. Reload the page to try to save again.')}
			</div>
			{showActions && (
				<div style={rowStyle}>
					<button type="button" style={buttonStyle} disabled={isResolving} onClick={handleOverwrite}>
						Overwrite
					</button>
					<button type="button" style={buttonStyle} disabled={isResolving} onClick={handleReload}>
						Reload
					</button>
				</div>
			)}
			{showStalledReload && (
				<div style={rowStyle}>
					{/* Bare reload only: resolveConflict(overwrite:false) drops the
					    room's retained scene server-side, and this is not a conflict.
					    Deliberately does not set reloading: this is not a discard, so
					    useSync's beforeunload PUT stays armed as the user's last
					    chance to save before the reload. */}
					<button type="button" style={buttonStyle} onClick={() => window.location.reload()}>
						Reload
					</button>
				</div>
			)}
		</div>
	)
}
