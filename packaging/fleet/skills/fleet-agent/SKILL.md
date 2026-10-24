---
name: fleet-agent
description: Use whenever you are a fleet agent and need to talk to someone (report, ask, progress note), use the operator's accounts (mail, calendar, WhatsApp, GitHub, Slack through the KARMAX gateway), receive files, handle a secret, or check what you may do — the full fleetctl command reference with examples and what to do when a command fails.
---

# fleetctl for a fleet agent

You are one of the operator's standby agents. `fleetctl` is how you reach
everyone and everything outside your container; fleetd (the fleet's daemon)
decides who may do what. Your CLAUDE.md has the rules in short; this is the
reference. Look a command up here instead of guessing.

Check that it works with `fleetctl whoami` or `fleetctl status`. Never send a
test message: a tell to the orchestrator wakes it for a real turn.

## Talking

Report to whoever gave you the task (the brief starts `Task <id> from <name>.`);
the orchestrator if it does not say.

```
fleetctl tell karmax "branch fleet/agent-05/topic, tests pass; one thing left: …"
fleetctl tell operator "…"                      # waits in the operator's inbox
fleetctl tell ram@example.com "…"               # a teammate: name the branch and PR
fleetctl tell karmax "halfway: the parser is done" --kind progress
fleetctl tell karmax "which of the two APIs should I keep?" --kind question
```

- `--kind report` (default) is a result. `progress` needs no reply. `question`
  asks without blocking you.
- Reports go through `fleetctl tell` even to a local orchestrator: that files
  them under the task. SendMessage does not, and a word to another agent is not a
  report.
- `queued` in the answer means fleetd or the receiver was away; the message is
  kept and delivered later. Do not send it again.

### Asking when you are stuck

```
fleetctl ask "Brief says 'the config file' but there are two (a.yaml, b.yaml). I would edit b.yaml, which the tests read. Is that right?" --timeout 30m
```

It blocks until the answer arrives and prints it; go on from it in the same
turn. Make the question self-contained: what you tried, what you would do by
default. Failure cases:

- exit 4: it timed out. The question stays filed, and a later answer comes as a
  message. Go on with your default, or report that you are blocked.
- "<you> has no task to ask about": you have no open task, so there is nobody to
  ask. Wait for work, or `fleetctl tell operator "<question>"`.
- A connection error: the question may still be filed; if it is answered you get
  the answer as a message. Do not ask it again.
- A wrong guess costs more than the wait. Ask rather than invent.

### Follow-ups and cancels

`Task <id> follow-up from <name>.` is more instructions: treat it like the
brief and report the same way. A cancel notice means stop at once, leave the
branch as it is, and report nothing further.

## Files

Files someone sends you arrive in `/work/inbox/<task>/`. Read them there.

<!-- v2-files: when `fleetctl cp` / `fleetctl sync` land, write their usage here
from that task's brief: sending a file to a peer or the operator, syncing a
directory, and what a refusal or a size limit looks like. -->

## The operator's accounts (KARMAX gateway)

Only on a task the operator, one of their sessions or the orchestrator gave
you. On a team member's task, fleetd refuses (403) and you must not reach the
operator's accounts by any other route.

```
fleetctl karmax tools                           # what you may call, each read or write
fleetctl karmax tools --json
fleetctl karmax call google.mail.search query="invoice" --reason "find the invoice named in the task"
fleetctl karmax call comms.send to="Ram" text="build is green" --reason "the task asks me to tell Ram when it is"
fleetctl karmax call some.tool limit:=5 draft:=true --reason "…"
fleetctl karmax call some.tool --json-args '{"query": "x", "limit": 5}' --reason "…"
```

- `k=v` is a string; `k:=5`, `k:=true`, `k:='[1,2]'` are JSON.
- **Reads are free** and run at once.
- **Writes** (send, change, delete, post) wait for approval. The orchestrator
  approves low-risk ones herself, and asks the operator on WhatsApp about
  anything destructive or that sends or deletes. Give a `--reason` an approver
  can decide on: what, to whom, why the task needs it.
- A write's call blocks (30 minutes by default, `--timeout`). Refused or
  expired, it exits 1 and prints why: do not retry the same call; adjust it, or
  `fleetctl ask`.
- **Denied outright**: tools that run code (shell, files, raw HTTP), memory
  writes, credentials, secrets, schedules and the fleet itself. Exit 1 at once.
- Ask only for what the task needs.

## Memory

On the operator's tasks. See the `fleet-memory` skill: `fleetctl memory recall`
before work the operator has history with, `fleetctl memory suggest` for durable
facts. Only the orchestrator saves.

## Secrets

Only through `fleetctl secret`. Never print a secret, commit one, write one in
a file in a repo, or put one in a message, report, question or `--reason`. If
you meet one by accident (a token in a log), leave it out of everything you
write.

```
fleetctl secret exec OPENAI_API_KEY=openai -- <cmd> [args]   # the secret in cmd's environment; nothing printed; cmd's exit code
fleetctl secret get deploy-key --to deploy_key               # a 0600 file under /work/.secrets (other paths refused)
```

The brief names a secret granted to you (never its value). Grants belong to a
task and end when it closes. Never use `--print` or `cat` the file: what you
print enters your context.

| Refusal | What it means | What to do |
|---|---|---|
| 403, not granted | no grant for your task or agent, or it expired | say so in your report; do not look for another way in |
| 403, not the operator's task | a team member gave this task | secrets are for the operator's tasks only |
| 403, does not verify | your `FLEET_AGENT_KEY` is missing or wrong | report it; the fleet needs a re-render |
| `--to` refused | the path is outside `/work/.secrets` or escapes it | use a plain name under it |

`kbuild` does not carry a secret to the build host: run what needs it here.

## Model and identity

```
fleetctl whoami                    # your scope and who fleetd knows you as
fleetctl model agent-05            # the model you run, its override, and the live one
```

fleetd knows you by a key in your environment (`FLEET_AGENT_KEY`); do not share
or print it. Giving `fleetctl model` a model name sets an agent's model; leave
that to whoever runs the fleet unless your brief tells you to.

## GitHub

`gh` and `git` pick the account from the repository: the fleet's by default,
the owner's own account for the owners listed in your CLAUDE.md. No other
account is available. Never set `GH_TOKEN` yourself or print a token. Never
push to `main`; never rebase or force-push a branch once you have reported it.

## kbuild

Builds and test suites go to the build host: `kbuild go test -race ./...` from
the repo or a worktree under the shared code. If your CLAUDE.md has no kbuild
section, this fleet has no build host.

## Public repos

A public repo has a pre-commit hook that refuses personal data. Use fake values
in code, tests, docs and commit messages: `ram@example.com`, hosts like `pc2` or
`kali`, addresses `192.0.2.x` or `100.64.0.x`, paths like `/home/op/code`. Never
commit a real name, login, phone number, IP or home path. Never `--no-verify`.

## When something fails

| You see | It means | Do |
|---|---|---|
| exit 4 from `ask` | nobody answered in time | go on with your default, or report blocked |
| exit 1 from `karmax call` | refused, expired, or denied outright | read the reason; adjust or ask |
| 403 | not allowed on this task (a team member's task, or a scope you lack) | stop; do not try another route |
| `queued` from `tell` | receiver away, message kept | nothing |
| connection refused | fleetd is away | wait a minute and retry once, then report |
