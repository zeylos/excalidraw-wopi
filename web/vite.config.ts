import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { viteStaticCopy } from 'vite-plugin-static-copy'

// The Excalidraw component reads fonts from window.EXCALIDRAW_ASSET_PATH at
// runtime (set in main.tsx), so the font files must land at that path in dist.
export default defineConfig({
  plugins: [
    react(),
    viteStaticCopy({
      targets: [
        {
          src: 'node_modules/@excalidraw/excalidraw/dist/prod/fonts/**/*',
          dest: 'fonts',
          // Each font family lives in its own subdirectory under fonts/;
          // strip the node_modules prefix but keep that per-family layout.
          rename: { stripBase: 6 },
        },
      ],
    }),
  ],
  worker: {
    format: 'es',
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'happy-dom',
  },
})
