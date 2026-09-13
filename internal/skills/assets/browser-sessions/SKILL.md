---
name: browser-sessions
description: Use when a task needs the live web — signing into a site, reading a page behind a login, or acting on one the operator is already signed into (Instagram, LinkedIn, Google, anything else they use) — through KARMAX's shared browser and the attached Playwright MCP server.
---

## One browser, not two

KARMAX runs a single headed Chromium, with a profile it owns and a DevTools
(CDP) endpoint on loopback — never the operator's daily Chrome. The operator
signs into whatever they want reachable — Google, Instagram, LinkedIn,
whatever — in that one window. Playwright MCP then attaches to that same
browser over CDP and finds those sessions already there. That's the whole
design: an agent given a browser of its own would be signed into nothing, and
a URL printed for the operator to open in their own daily browser would put
the session somewhere the agent can never reach. One shared window is what
lets the operator complete a sign-in step and the agent act on it, without a
handoff.

The CDP endpoint being bound to loopback means any process running as this
user can drive it — worth knowing, not a boundary to lean on.

## Two different tools, don't confuse them

- **`browser`** (a KARMAX tool, see the `karmax-tools` skill) controls the
  window itself: `open` a URL and raise it in front of the operator (the
  right move whenever a flow needs *them* to sign in or click something —
  then tell them what to do there), `status`, or `start` it with nothing
  navigated. It has no `stop`/`close` action.
- **Playwright MCP** (server name `playwright`, launched pointed at the
  browser's CDP endpoint, reconfigured fresh on every harness invocation —
  never written into persistent settings) is how you actually drive a page.
  If the browser isn't running, this server isn't attached and none of its
  tools exist yet — check/start it with `browser` first.

Playwright MCP tools you have: navigation and page state
(`browser_navigate`, `browser_navigate_back`, `browser_tabs`,
`browser_resize`, `browser_wait_for`); reading what's on screen
(`browser_snapshot`, `browser_take_screenshot`, `browser_find`,
`browser_console_messages`); acting on it (`browser_click`, `browser_type`,
`browser_hover`, `browser_drag`, `browser_drop`, `browser_select_option`,
`browser_fill_form`, `browser_press_key`, `browser_file_upload`,
`browser_handle_dialog`); inspecting traffic (`browser_network_request`,
`browser_network_requests`); and an escape hatch (`browser_evaluate`,
`browser_run_code_unsafe`). `browser_close` closes a page/tab, not the
browser session itself.

## The browser being open is the grant

There's no separate consent step here, no per-site token, no scope to check.
Whatever the operator has signed into is reachable the instant the window is
open — that's the entire access model. Symmetrically: **closing the browser
revokes it, and nothing else has to happen.** No token to invalidate, no
per-site logout first — the window closing *is* the revocation.

Two things follow. First, you have no way to end this yourself — the
`browser` tool has no close/stop action, on purpose. Access ends only when
the operator closes the window; don't suggest logging out of individual
sites as a substitute, and don't treat their opening the browser as
standing permission to act on every site the profile happens to be signed
into rather than whatever they opened it for. Second, if you need the window
open for a step only the operator can complete (an OTP, a captcha, an
approval click), say so and hand it back with `browser` `open` — don't try
to script around it.

## The httpOnly trap

`document.cookie` — whether read through page JavaScript or
`browser_evaluate` — **cannot see httpOnly cookies.** Instagram's `sessionid`
is one. Code that reads `document.cookie` to harvest a session will run
without error and hand back a string that looks plausible, just missing the
one cookie that mattered — and whatever consumes it will fail downstream in
a way that won't look like a cookie problem.

Two ways around it, both verified against this instance:

- `browser_run_code_unsafe` running `page.context().cookies()` — Playwright's
  own API, backed by CDP rather than in-page JS, so it isn't subject to the
  httpOnly restriction.
- `browser_network_request` with `part: "request-headers"` on a request
  that's already been captured — the `Cookie` header it sent carries
  everything, httpOnly included.

Reach for one of these, not `document.cookie`, any time the actual value of
a session cookie is what you need rather than just its presence.
