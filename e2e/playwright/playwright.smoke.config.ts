import { defineConfig, devices } from '@playwright/test'

// The PR gate, against the real dockerized Drive stack `make e2e-up`
// brings up (our binary on :8080, Drive on :8000; see e2e/README.md).
// Unlike playwright.config.ts's local suite, this file starts no
// webServer of its own: `make e2e-smoke` checks the
// stack is already up before invoking Playwright, since bringing it up
// takes several minutes and must not happen on every test run.
export default defineConfig({
  testDir: './specs/smoke',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  // The 30s Playwright default undercuts this suite's own worst-case waits
  // (SAVE_POLL_TIMEOUT_MS and fixtures/drive.ts's WOPI_POLL_TIMEOUT_MS, both
  // 60-75s), and beforeAll alone (two logins plus two wopiLaunch polls)
  // can approach it. expect's own timeout is raised to match, so a single
  // `expect(...).toPass()`-style assertion is not left on the 5s default.
  timeout: 120_000,
  expect: { timeout: 20_000 },
  reporter: process.env.CI ? [['list'], ['html', { open: 'never' }]] : [['list']],
  use: {
    baseURL: 'http://localhost:8080',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
})
