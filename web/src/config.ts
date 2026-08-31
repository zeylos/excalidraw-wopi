/**
 * AppConfig holds the launch parameters the Go server injects into the
 * `#ew-config` JSON blob when it serves the editor page.
 */
export interface AppConfig {
  fileId: string
  fileName: string
  userId: string
  userName: string
  canWrite: boolean
  sessionToken: string
  apiBase: string
  socketPath: string
  /**
   * The operator's EXCALIDRAW_WOPI_MAX_IMAGE_BYTES: the one env-configurable
   * image size limit, enforced client-side on both the sending path
   * (useSync) and the receiving path (useCollaboration).
   */
  maxImageBytes: number
  /**
   * Enables window.__excaTest (see stores/useExcalidrawStore.ts), the
   * Playwright suite's scene accessor. Optional; defaults to false when the
   * Go server does not set it.
   */
  testHooks?: boolean
}

/**
 * loadConfig reads the `#ew-config` blob and parses it into an AppConfig.
 * It returns null when the blob is empty or malformed, which is the case
 * in dev mode before the Go server injects real values.
 */
export function loadConfig(): AppConfig | null {
  const el = document.getElementById('ew-config')
  if (!el || !el.textContent) {
    return null
  }

  const text = el.textContent.trim()
  if (text === '') {
    return null
  }

  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch (err) {
    console.error('ew-config blob holds malformed JSON', err)
    return null
  }

  const fileId = (parsed as { fileId?: unknown } | null)?.fileId
  if (typeof fileId !== 'string' || fileId === '') {
    console.error('ew-config blob is missing a non-empty fileId field')
    return null
  }

  return parsed as AppConfig
}
