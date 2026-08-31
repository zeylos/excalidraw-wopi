import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    globalSetup: "./global-setup.mjs",
    testTimeout: 10000,
    hookTimeout: 20000,
    fileParallelism: false,
  },
});
