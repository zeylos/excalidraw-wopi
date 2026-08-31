/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// The icon set (@mdi/react, @mdi/js) and the translation helper
// (@nextcloud/l10n) are not dependencies of this project; this component
// uses plain text and inline styles instead. The "Open Admin Settings"
// action is dropped: this project ships no admin settings page (the
// session JWT is minted once, at launch, from a server secret with no
// admin UI to reconfigure it), so a persistent jwt_secret_mismatch has no
// page to link to. Plain color values replace the CSS custom properties a
// host stylesheet would otherwise supply, since this project has none.

import { useEffect, useState, memo, useCallback, type CSSProperties } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import type { AuthErrorType } from '../stores/useCollaborationStore'

interface AuthErrorConfig {
	title: string
	message: string
	color: string
	background: string
}

const getAuthErrorConfig = (errorType: AuthErrorType): AuthErrorConfig | null => {
	switch (errorType) {
	case 'jwt_secret_mismatch':
		// The session JWT is minted once, at launch, with no
		// refresh path: this error is always terminal for the session, so the
		// notification never promises a retry that will not happen. Reopen the
		// board from the WOPI host to start a new session.
		return {
			title: 'Authentication configuration issue',
			message: 'The app cannot connect to the collaboration server. Your changes save to this device.',
			color: '#991b1b',
			background: '#fef2f2',
		}
	default:
		// 'token_expired' and 'unauthorized' are unreachable: nothing in this
		// codebase ever sets authError.type to either (production only calls
		// markTerminalAuthFailure('jwt_secret_mismatch', ...)).
		return null
	}
}

const containerStyle = (color: string, background: string): CSSProperties => ({
	position: 'fixed',
	top: 16,
	insetInlineEnd: 16,
	zIndex: 10000,
	maxWidth: 380,
	minWidth: 280,
	padding: 16,
	borderRadius: 8,
	boxShadow: '0 4px 12px rgba(0, 0, 0, 0.15)',
	border: `1px solid ${color}`,
	color,
	backgroundColor: background,
})

const AuthErrorNotificationComponent = () => {
	const { authError, clearAuthError } = useCollaborationStore(
		useShallow(state => ({
			authError: state.authError,
			clearAuthError: state.clearAuthError,
		})),
	)

	const [isDismissed, setIsDismissed] = useState(false)

	const shouldShow = authError.type !== null && authError.isPersistent && !isDismissed

	useEffect(() => {
		if (shouldShow && !authError.isPersistent) {
			const timer = setTimeout(() => setIsDismissed(true), 8000)
			return () => clearTimeout(timer)
		}
	}, [shouldShow, authError.isPersistent])

	const handleDismiss = useCallback(() => {
		setIsDismissed(true)
		if (!authError.isPersistent) {
			clearAuthError()
		}
	}, [authError.isPersistent, clearAuthError])

	if (!shouldShow) {
		return null
	}

	const config = getAuthErrorConfig(authError.type)
	if (!config) {
		return null
	}

	return (
		<div style={containerStyle(config.color, config.background)}>
			<div style={{ display: 'flex', alignItems: 'flex-start', gap: 12 }}>
				<div style={{ flex: 1, minWidth: 0 }}>
					<div style={{ fontWeight: 600, fontSize: 14, marginBottom: 4 }}>{config.title}</div>
					<div style={{ fontSize: 13, lineHeight: 1.4 }}>{config.message}</div>
					{authError.isPersistent && (
						<div style={{ fontSize: 12, opacity: 0.8, marginTop: 8 }}>
							Local changes save automatically to this device.
						</div>
					)}
				</div>
				<button
					type="button"
					onClick={handleDismiss}
					title="Dismiss"
					aria-label="Dismiss"
					style={{
						background: 'none',
						border: 'none',
						padding: 4,
						borderRadius: 4,
						cursor: 'pointer',
						color: config.color,
						fontSize: 16,
						lineHeight: 1,
					}}
				>
					×
				</button>
			</div>
		</div>
	)
}

export const AuthErrorNotification = memo(AuthErrorNotificationComponent)
