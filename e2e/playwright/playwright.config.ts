import { defineConfig, devices } from '@playwright/test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const binaryPath = path.join(repoRoot, 'bin', 'excalidraw-wopi')

// Work package 4.5: a fast local suite that needs no Drive and no
// docker. The webServer starts the real Go binary with the in-process
// fake WOPI host turned on (EXCALIDRAW_WOPI_FAKE_HOST=1); reuseExistingServer
// stays false so a stale binary from a previous run, still bound to the
// port, never masks a build that failed to pick up a code change.
export default defineConfig({
  testDir: './specs/local',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  // CI additionally writes the HTML report so the workflow can upload it
  // as an artifact on failure; a local run only needs the terminal list.
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: 'http://localhost:8085',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: binaryPath,
    url: 'http://localhost:8085/healthz',
    reuseExistingServer: false,
    timeout: 30_000,
    env: {
      EXCALIDRAW_WOPI_FAKE_HOST: '1',
      EXCALIDRAW_WOPI_LISTEN_ADDR: ':8085',
      EXCALIDRAW_WOPI_PUBLIC_URL: 'http://localhost:8085',
      // A fixed dev-only secret keeps repeat runs from generating a new
      // random one every start; the value has no security role here,
      // since --fake-host mode carries no real WOPI host credentials.
      EXCALIDRAW_WOPI_SESSION_SECRET: 'e2e-playwright-local-fixed-session-secret-32b',
      EXCALIDRAW_WOPI_PROOF_KEY_PATH: path.join(repoRoot, 'e2e', 'playwright', '.run', 'proof-key.pem'),
    },
  },
})
