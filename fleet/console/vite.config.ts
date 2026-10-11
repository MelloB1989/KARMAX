import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

// VITE_CONSOLE points the dev server at a running fleet-console, e.g. through
// `ssh -L 7880:127.0.0.1:7880 pc2`. The production build is embedded in the
// Go binary and served by fleet-console itself.
const target = process.env.VITE_CONSOLE;

export default defineConfig({
  plugins: [react()],
  build: { outDir: "../internal/console/dist", emptyOutDir: true },
  server: target
    ? { proxy: { "/v1": { target, ws: true }, "/login": { target } } }
    : undefined,
  test: { environment: "jsdom" },
});
