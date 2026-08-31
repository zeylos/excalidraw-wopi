// SPDX-License-Identifier: AGPL-3.0-or-later

// This is the entry point the Go server's embedded index.html loads; it
// stays a thin DOM/store wiring shell, and defers the ready/not-ready
// decision to bootstrap.ts's resolveBootstrap so that logic stays
// unit-testable without mounting React.

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './styles/base.css'
import App from './App'
import { loadConfig } from './config'
import { resolveBootstrap } from './bootstrap'
import { useSessionStore } from './stores/useSessionStore'
import { useWhiteboardConfigStore } from './stores/useWhiteboardConfigStore'

// A missing or malformed #ew-config blob means this page was opened
// directly, not through the Go server's /launch endpoint (every launch
// mints a fresh session JWT and injects it into that blob).
function LaunchRequired() {
  return (
    <div style={{ height: '100vh', width: '100vw', display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <p>This page must be opened from your document host.</p>
    </div>
  )
}

const container = document.getElementById('root')
if (!container) {
  throw new Error('root element not found')
}

const decision = resolveBootstrap(loadConfig())

if (decision.ready) {
  useSessionStore.getState().setSession(decision.session)
  useWhiteboardConfigStore.getState().setConfig(decision.whiteboardConfig)
}

createRoot(container).render(
  <StrictMode>
    {decision.ready
      ? (
          <App
            apiBase={decision.appProps.apiBase}
            socketPath={decision.appProps.socketPath}
            maxImageBytes={decision.appProps.maxImageBytes}
          />
        )
      : <LaunchRequired />}
  </StrictMode>,
)
