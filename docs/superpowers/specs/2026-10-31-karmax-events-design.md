# Every account's events through one decision core

Date: 2026-10-31. Written for an overnight build by parallel agents. The operator
was asleep, so the decisions below are recorded as they made them before
leaving.

## Intent

KARMAX should hear about everything that happens on the operator's accounts,
not only WhatsApp. The flow the operator described:

```
events from every account ─▶ KARMAX (Jev / reflex decision core)
                           ─▶ GitLoom memory updated with what is durable
                           ─▶ Jev decides what to do, and KARMAX does it
```

KARMAX reaches the operator only on their personal WhatsApp channel, for
approvals and urgent things.

## Sources

| Source | Accounts | How (first cut) |
|---|---|---|
| WhatsApp | linked account | wacli webhook (exists) |
| GitHub | every account `gh auth status` lists (several) | poll notifications per account with `gh auth token --user` |
| Google Calendar | every Google account `gws`/`gog` can use (one now, more in the morning) | poll changes and upcoming events |
| Google Chat | same Google accounts | poll spaces/DMs for new messages |
| Slack | the configured workspace(s) | the existing connector; events or polling |
| AWS | every usable profile in `~/.aws/config` | CloudWatch alarms in ALARM; daily cost and anomalies (Cost Explorer / budgets) |
| PostHog | API key when connected | alerts/insight thresholds, new error spikes |

A source that has no credentials yet stays off and says so in its health
check; connecting an account in the morning turns it on without a code change.

## Decided by the operator

- **Autonomy: act freely, tell after.** KARMAX may act on events without asking,
  including writing to other people as the operator (WhatsApp, Slack, GitHub
  comments, email), and reports afterwards what it sent.
- **WhatsApp volume: approvals and urgent only.** No digests. Routine actions
  are not reported over WhatsApp; messages sent as the operator are (that is
  the "tell after"), and so is anything urgent.
- **Identity.** KARMAX has taken the operator for a co-founder (in a group chat
  and once in the personal channel), and earlier for the legacy account name.
  It must never confuse the operator with anyone else.
- Overnight the build may push to main, restart the daemon, ship the GitLoom
  extraction fix to production, and repair memory the extractor damaged.

## Shape (builder's calls)

- Every source is a package under `internal/sources/<name>` that turns what
  happened into `sources.Event` and publishes `Event.BusEvent(agentID)`: a
  `webhook.fired` event with route `source:account`. The router, reflex
  screening, memory filing and agent turn already handle that kind, so a
  source needs no new routing.
- Polling sources run as loopkit loops registered from their own
  `internal/runtime/<name>source.go`, dedupe with `sources.Seen` over the KV
  store, and start where they left off after a restart.
- Credentials come from the CLIs already logged in on the host (`gh`, `gws`,
  `gog`, `aws`) or KARMAX's own connector store; never printed or logged.
- Hard floor kept despite "act freely" (my call, to be confirmed by the
  operator): no deletion of repositories, data or cloud resources, no
  payments, and no sharing of secrets without asking on WhatsApp.

## Non-goals

- New public webhooks or tunnels; polling is enough for a single host.
- A UI for the sources; health shows up in the existing integration health.

## Anti-goals

- The operator mistaken for anyone else, anywhere.
- WhatsApp noise: an FYI the operator did not need.
- A source that silently stops; it must surface in health.
