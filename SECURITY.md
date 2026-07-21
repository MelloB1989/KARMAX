# Security Policy

## Reporting a vulnerability

Please report security issues **privately** via GitHub's
[Report a vulnerability](https://github.com/MelloB1989/karmax/security/advisories/new)
(Security → Advisories), not as a public issue. Include reproduction steps and
the affected commit.

## Threat model — read before running

KARMAX is an autonomous agent that, by design, runs with **broad access** to the
host and connected accounts (WhatsApp via wacli, Google Workspace, the shell, and
delegated coding harnesses). Treat a KARMAX instance as a privileged credential.

Key protections built in:

- **Human-in-the-loop approvals** — money-spending, public posts, and
  destructive actions are surfaced as approvals in the app, never executed
  directly.
- **Scoped WhatsApp access** — only chats explicitly added to the wacli webhook
  are monitored; webhook deliveries are HMAC-verified.
- **Locked chats / DND** — enforced at the wacli layer.

## Secrets & configuration

- Runtime secrets live in `karmax.yaml` and `.env`, both **gitignored** — never
  commit them. Model/provider keys, the WhatsApp webhook HMAC secret, and API
  tokens belong there, not in code.
- The local state directory (`~/.karmax`, SQLite memory + logs) is **unencrypted
  at rest**; protect it like a credential store.

## Responsible use

Pointing KARMAX at real accounts means an LLM can act on your behalf. Review the
config and tool allowlist, start with test accounts, and keep the approval flow
enabled for anything consequential.
