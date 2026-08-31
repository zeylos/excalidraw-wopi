// Ambient type for window.__excaTest, the read-only scene accessor
// web/src/stores/useExcalidrawStore.ts exposes. This project has no
// build-time dependency on web/'s TypeScript project, so the shape is
// restated here rather than imported.
export {}

declare global {
  interface Window {
    __excaTest?: {
      getElements: () => readonly unknown[]
      getAppState: () => { viewModeEnabled?: boolean }
      getFiles: () => Record<string, { id: string, dataURL: string, mimeType: string }>
      getCollabState: () => { status: 'online' | 'offline' | 'connecting' | 'reconnecting', isInRoom: boolean }
    }
  }
}
