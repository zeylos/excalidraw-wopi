import { defineConfig } from "vitest/config";

// No globalSetup here: `make e2e-nightly` depends on `make e2e-smoke`,
// which already guards that `make e2e-up` ran, so this suite does not
// stand up its own stack.
export default defineConfig({
  test: {
    // A syncer-failover or reconnect-storm run walks several rounds, each
    // with its own settle window and a save-landing poll bounded by the
    // 60s server-save throttle; a single test can approach several
    // minutes.
    testTimeout: 300_000,
    hookTimeout: 120_000,
    fileParallelism: false,
  },
});
