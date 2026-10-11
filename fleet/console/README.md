# Fleet console panel

React + three.js panel, embedded in the `fleet-console` binary (`../internal/console/dist`).

- `npm run dev` — dev server; `VITE_FIXTURE=1 npm run dev` replays `dev/` fixtures (dev only, never in a build).
- `npm test` — unit tests (vitest, jsdom).
- `npm run e2e` — end-to-end checks of the **production build** in headless Chromium. Run it before reporting UI work.

## End-to-end (`e2e/`)

`npm run e2e` builds the panel, installs Chromium for Playwright if it is missing, and runs `e2e/panel.spec.ts`.
`e2e/replay-server.mjs` is a test-only Node server (never shipped, never imported by `src/`): it serves the built
`dist`, accepts `/login?k=e2e-token`, and answers `/v1/watch` with `dev/snapshot.json` (every timestamp shifted so the
snapshot's `at` is now, re-sent each second, so nothing reads stale) and `/v1/term/<agent>[?control=1]` with
`dev/term-<agent>.ans` and a `hello` frame. It binds `127.0.0.1` on a free port; `e2e/global-setup.mjs` starts it.

The checks: the boot log is gone within 3 s; four agents with phase chips; the centre terminal has non-empty rows
(WebGL is switched off for that test so xterm's DOM rows can be read); `F` enters Focus and `Esc` leaves; `Enter` shows
"YOU HAVE THE KEYBOARD" and `Ctrl+]` removes it; `O` opens the office and each of the five desk screens has painted
pixels; no `console.error` or page error in any test. Each test saves `e2e-<name>.png` into `$E2E_SHOTS` (default `e2e/screens`, git-ignored).

On the build host: `cd fleet/console && E2E_SHOTS=<dir> kbuild "npm run e2e"` (the browser is downloaded each run there). Chromium
uses software GL (SwiftShader), so the office renders at a few fps and the governor turns bloom off; the tests wait
for state, not frames.
