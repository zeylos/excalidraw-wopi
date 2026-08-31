import { defineConfig, devices } from '@playwright/test'

// The nightly suite: slow, real-time scenarios (conflict resolution,
// session-token expiry, WOPI lock TTL survival) against the real
// dockerized Drive stack `make e2e-up` brings up (our binary on :8080,
// Drive on :8000; see e2e/README.md). Like playwright.smoke.config.ts,
// this file starts no webServer of its own.
//
// specs/nightly's file names sort 01/02/03 on purpose: the ~36-minute
// lock-TTL spec (03) runs last, after the faster conflict (01) and
// token-expiry (02) specs already proved the quicker paths.
export default defineConfig({
  testDir: './specs/nightly',
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  // The lock-TTL spec calls test.setTimeout(45 * 60_000) itself, well past
  // this default; every other nightly spec finishes in a few minutes, so
  // this default only needs to clear those.
  timeout: 300_000,
  expect: { timeout: 20_000 },
  // A distinct outputDir and html outputFolder from the smoke stage's
  // config: both default to the same dirs, and Playwright clears its
  // output dir at the start of a run, so a shared name would wipe the
  // smoke stage's report.
  outputDir: './test-results-nightly',
  reporter: process.env.CI ? [['list'], ['html', { open: 'never', outputFolder: './playwright-report-nightly' }]] : [['list']],
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
