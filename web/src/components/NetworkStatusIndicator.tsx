/**
 * SPDX-FileCopyrightText: 2025 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

// The icon set (@mdi/react, @mdi/js) and the translation helper
// (@nextcloud/l10n) are not dependencies of this project, so this
// component uses plain text labels and inline styles instead. Plain color
// values replace the CSS custom properties a host stylesheet would
// otherwise supply, since this project has no host page supplying those
// tokens.

import { useEffect, useState, memo, useCallback, useMemo, useRef, type CSSProperties } from 'react'
import { useShallow } from 'zustand/react/shallow'
import { useCollaborationStore } from '../stores/useCollaborationStore'
import type { CollaborationConnectionStatus } from '../stores/useCollaborationStore'

interface StatusConfig {
	text: string
	color: string
	description: string
}

const getStatusConfig = (status: CollaborationConnectionStatus): StatusConfig => {
	switch (status) {
	case 'offline':
		return { text: 'Offline', color: '#f44336', description: 'Offline. Changes save locally.' }
	case 'connecting':
		return { text: 'Connecting', color: '#ff9800', description: 'Connecting to the collaboration server.' }
	case 'reconnecting':
		return { text: 'Reconnecting', color: '#ff9800', description: 'The app tries to reconnect.' }
	case 'online':
		return { text: 'Online', color: '#4caf50', description: 'Connected.' }
	}
}

const containerStyle = (color: string, expanded: boolean): CSSProperties => ({
	position: 'absolute',
	bottom: 16,
	insetInlineEnd: 16,
	zIndex: 10000,
	display: 'flex',
	alignItems: 'center',
	gap: 8,
	padding: expanded ? '8px 12px' : 8,
	borderRadius: 8,
	cursor: 'pointer',
	fontSize: 13,
	fontWeight: 600,
	color,
	backgroundColor: `${color}26`,
	border: `1px solid ${color}66`,
})

const NetworkStatusIndicatorComponent = () => {
	const { status, authError } = useCollaborationStore(
		useShallow(state => ({
			status: state.status,
			authError: state.authError,
		})),
	)
	const [expanded, setExpanded] = useState(false)
	const prevStatusRef = useRef(status)

	useEffect(() => {
		if (prevStatusRef.current !== status) {
			prevStatusRef.current = status
			setExpanded(true)
			const timeout = setTimeout(() => setExpanded(false), 3000)
			return () => clearTimeout(timeout)
		}
	}, [status])

	const statusConfig = useMemo(() => getStatusConfig(status), [status])
	const { text, color, description } = statusConfig

	const enhancedDescription = useMemo(() => {
		if (authError.isPersistent) {
			return `${description} Authentication configuration issue detected.`
		}
		return description
	}, [description, authError])

	const toggleExpanded = useCallback(() => setExpanded(prev => !prev), [])

	const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
		if (e.key === 'Enter' || e.key === ' ') {
			e.preventDefault()
			toggleExpanded()
		}
	}, [toggleExpanded])

	if (status === 'online') return null

	return (
		<div
			style={containerStyle(color, expanded)}
			onClick={toggleExpanded}
			onKeyDown={handleKeyDown}
			title={enhancedDescription}
			role="button"
			aria-live="polite"
			aria-label={`Connection: ${text}. ${expanded ? enhancedDescription : 'Click to expand.'}`}
			tabIndex={0}
		>
			<span aria-hidden="true" style={{ width: 8, height: 8, borderRadius: '50%', backgroundColor: color, flexShrink: 0 }} />
			{expanded && <span>{text}</span>}
		</div>
	)
}

export const NetworkStatusIndicator = memo(NetworkStatusIndicatorComponent)
NetworkStatusIndicator.displayName = 'NetworkStatusIndicator'
