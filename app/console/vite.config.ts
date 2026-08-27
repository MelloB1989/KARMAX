import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// Builds to dist/ as static assets the karmax binary embeds — see
// app/console/API.md for what the Go side needs to do with the output.
export default defineConfig({
  // Absolute, not "./". The console is a client-routed SPA served under a
  // history fallback: from /cases/case_c1 a relative asset URL resolves to
  // /cases/assets/…, the fallback answers it with index.html, and the browser
  // refuses the HTML as a module script. Every deep link and every refresh on
  // a nested route dies that way.
  base: "/",
  plugins: [react(), tailwindcss()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "./src") } },
  server: { port: 5179, host: true },
  build: { outDir: "dist", emptyOutDir: true },
});
