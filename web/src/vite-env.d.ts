/// <reference types="vite/client" />

// The editor sets this path before it loads @excalidraw/excalidraw, so the
// component reads its font files from the right URL.
declare global {
  interface Window {
    EXCALIDRAW_ASSET_PATH: string
  }
}

export {}
