import { defineConfig } from "@playwright/test";

// Run through `npm run e2e`, which builds the panel first. Chromium uses software GL (SwiftShader),
// so the office renders at a few fps here; the tests wait for state, not for frames.
export default defineConfig({
  testDir: "e2e",
  testMatch: "*.spec.ts",
  globalSetup: "./e2e/global-setup.mjs",
  workers: 1,
  timeout: 60_000,
  reporter: "list",
  use: {
    viewport: { width: 1920, height: 1080 },
    launchOptions: {
      args: ["--use-gl=angle", "--use-angle=swiftshader", "--enable-unsafe-swiftshader", "--ignore-gpu-blocklist"],
    },
  },
});
