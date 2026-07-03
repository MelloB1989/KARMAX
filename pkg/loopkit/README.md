# loopkit — author KARMAX loops in Go

`loopkit` is the public SDK for building **KARMAX loops**: recurring units of
work (digests, syncs, watchers, reminders) authored as real Go code instead of a
one-line prompt in `karmax.yaml`. A loop gets access to KARMAX's capabilities —
the agent, long-term memory, the Claude Code harness, push notifications,
WhatsApp, and HTTP — through the `Kit` interface.

```
go get github.com/MelloB1989/karmax/pkg/loopkit
```

## Write a loop

Create a Go module and register your loop(s) in `init()`:

```go
package hndigest

import (
	"context"
	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func init() {
	loopkit.Register(loopkit.Loop{
		Name:        "hn-digest",
		Description: "Posts a Hacker News digest to the app every morning.",
		Schedule:    loopkit.Cron("0 0 8 * * *"), // 08:00 daily (sec min hour dom mon dow)
		Run: func(ctx context.Context, k loopkit.Kit) error {
			body, _, err := k.HTTP(ctx, "GET",
				"https://hacker-news.firebaseio.com/v0/topstories.json", nil, "")
			if err != nil {
				return err
			}
			digest, err := k.Harness(ctx, "Summarize these HN items into 5 bullets:\n"+body)
			if err != nil {
				return err
			}
			_ = k.Remember(digest)
			return k.Notify("Hacker News digest", digest)
		},
	})
}
```

Schedules: `loopkit.Cron("0 */15 * * * *")` (cron **with seconds**) or
`loopkit.Every("2h")`.

## The Kit capabilities

| Method | What it does |
|---|---|
| `Ask(ctx, prompt)` | Run through the main agent (full toolset + memory + judgement). Uses the agent's model budget. |
| `Harness(ctx, prompt)` | Run directly through the Claude Code CLI (web/file/shell) — great for web research, independent of the main model's rate limits. |
| `Remember(fact)` | Store a durable fact in long-term memory. |
| `Recall(query, limit)` | Semantic search over memory. |
| `Notify(title, body)` | Push to the phone app (saved to the in-app feed **and** delivered as a push). Informational only. |
| `Propose(title, summary, action)` | File a pending APPROVAL in the operator's approvals inbox (with a push). Use for anything needing a decision — on approval, `action` is handed to the agent to execute as written. |
| `Remind(title, due, notes)` | Create a reminder on the operator's phone (additive, no approval). Use for things only the operator can personally do. `due` is optional ISO-8601. |
| `ReadWhatsApp(ctx, chat, limit)` | Recent WhatsApp messages (by chat name/JID, or "" for latest). |
| `HTTP(ctx, method, url, headers, body)` | Make an HTTP request. |
| `Config(key)` | Install-time value from `KARMAX_LOOP_<NAME>_<KEY>` env. |
| `Logf(format, args...)` | Write to KARMAX's logs. |

Loops depend **only** on this interface — never on KARMAX internals — so they
stay decoupled and compile against just this package.

## Publish to the marketplace

```bash
karmax loops new my-loop       # scaffold: loop.go + loop.json manifest + README
# implement Run(), fill in the description, then:
karmax loops publish my-loop   # validates + compiles, then submits to the registry
```

The registry is a public GitHub repo (`$KARMAX_LOOPS_REGISTRY`, default
`MelloB1989/karmax-loops`) rendered as a website at
https://mellob1989.github.io/karmax-loops/. Your loop's code can live in the
registry itself (default — no repo of your own needed) or in your own module
(`karmax loops new my-loop --module github.com/you/karmax-my-loop`, then only
the manifest is submitted). Consumers run `karmax loops install my-loop`.

## Install (for KARMAX operators)

KARMAX is statically compiled, so a loop is blank-imported into the binary. Run:

```
karmax loops          # TUI: install / remove / list
karmax loops list     # headless listing
```

In the TUI, choose **Install a loop**, paste the module path, and it will
`go get` the module, add the import, rebuild KARMAX, and offer to restart.
Under the hood it edits `internal/installedloops/installed.go` between the
`karmax:loops` markers and runs `go build` — so an install is a managed rebuild
(~30s), the standard approach for Go compile-time plugins.

If a loop needs secrets/config, set them in `.env`:
`KARMAX_LOOP_HN_DIGEST_APIKEY=...` → read via `k.Config("apikey")`.
