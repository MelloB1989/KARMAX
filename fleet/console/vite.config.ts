import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { copyFileSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const outDir = resolve(here, "../internal/console/dist");

// emptyOutDir wipes the two tracked files go:embed needs (.gitignore, .keep);
// put them back after every build.
const keepTracked = {
  name: "keep-tracked-dist-files",
  closeBundle() {
    mkdirSync(outDir, { recursive: true });
    for (const f of [".gitignore", ".keep"]) copyFileSync(resolve(here, "dist-tracked", f), resolve(outDir, f));
  },
};

// VITE_CONSOLE points the dev server at a running fleet-console, e.g. through
// `ssh -L 7880:127.0.0.1:7880 pc2`. The production build is embedded in the
// Go binary and served by fleet-console itself.
const target = process.env.VITE_CONSOLE;

export default defineConfig({
  plugins: [react(), keepTracked],
  build: { outDir, emptyOutDir: true },
  server: target
    ? { proxy: { "/v1": { target, ws: true }, "/login": { target } } }
    : undefined,
  test: { environment: "jsdom", exclude: ["e2e/**", "node_modules/**"] }, // e2e/ is Playwright's
});
