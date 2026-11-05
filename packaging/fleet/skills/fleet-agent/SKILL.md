---
name: fleet-agent
description: Use whenever you are a fleet agent and need to talk to someone (report, ask, progress note), run anything heavier than a small test or build (a full suite, a big build, data processing, training: never in your container), use the operator's accounts (mail, calendar, WhatsApp, GitHub, Slack through the KARMAX gateway), receive files, handle a secret, make or remove a worktree (wt), or check what you may do — the full fleetctl command reference with examples and what to do when a command fails.
---

# fleetctl for a fleet agent

You are one of the operator's standby agents. `fleetctl` is how you reach
everyone and everything outside your container; fleetd (the fleet's daemon)
decides who may do what. Your CLAUDE.md has the rules in short; this is the
reference. Look a command up here instead of guessing.

Check that it works with `fleetctl whoami` or `fleetctl status`. Never send a
test message: a tell to the orchestrator wakes it for a real turn.

## Heavy jobs never run in this container

**Important.** Your container is for writing code and running small tests and
builds: it has a CPU or two and a few GB, shared with your sibling slots. A
full test suite, a big build, data processing, training, a long benchmark or
anything else compute-heavy runs elsewhere, in this order:

1. **`kbuild <cmd>`**: on the fleet host, in the fleet image, as you, in this
   directory, under the shared code (it sees it at the same path; it cannot
   see `/work`). Nothing to copy: `kbuild go test -race ./...`,
   `kbuild "npm ci && npm run build"`. Builds and test suites only: a heavy
   job never goes through kbuild (see the rule below).
2. **ssh to the fleet host**, for a heavy job, one that must outlive your
   turn, or files outside the shared code. You log in to its build account,
   which cannot read the shared code: copy what the job needs.
   ```
   h=${DOCKER_HOST#ssh://}                                  # the build account on the fleet host
   ssh "$h" mkdir -p jobs/<task> && rsync -a ./job/ "$h":jobs/<task>/
   ssh "$h" 'cd jobs/<task> && nohup flock ~/heavy.lock nice ./run.sh > run.log 2>&1 < /dev/null &'
   ssh "$h" 'tail -5 jobs/<task>/run.log'                    # check back; do not sit in a loop
   rsync -a "$h":jobs/<task>/out/ ./out/ && ssh "$h" 'rm -rf jobs/<task>'   # always clean up
   ```
   It has docker, git, python3, make and rsync; plain `docker` here runs there
   too, and `run.sh` may `docker run` an image. The host also runs the fleet:
   stay within a few cores (`nice`, `docker run --cpus 4 --memory 6g`) and
   never fill its disk.

   **One heavy job at a time on the fleet host, fleet-wide.** It has 12 cores
   and 16 GB for every agent's container as well: seven 4K renders at once
   once filled its memory and swap and stalled the whole fleet. A heavy job
   (a render or encode, data processing, training, a long benchmark: several
   cores or GBs for more than about 10 minutes) always starts under
   `flock ~/heavy.lock`, as above, so it waits behind the one running. A
   queued job's `run.log` stays empty until its turn; `ssh "$h" 'flock -n
   ~/heavy.lock true && echo free || echo busy'` says whether one is running.
   Inside a job, run its steps one after another, never several in parallel.
   Never kill, restart or duplicate a job that is waiting or paused (state
   `T`): it is queued, not hung. If waiting would take too long, or the work
   is many heavy jobs, ask for an EC2 instance (3) instead.
3. **A temporary EC2 instance**, for more than the host can spare (many cores,
   lots of memory, a GPU, hours of compute). Ask whoever gave you the task:
   ```
   fleetctl ask "I need an EC2 instance for <job>: <instance type, or cores and memory>, about <N> h, because <why>"
   ```
   The answer names the instance (`ubuntu@<ip>`, passwordless sudo) and the
   secret that holds its ssh key: `fleetctl secret get <secret> --to ec2-key`,
   then `ssh -i /work/.secrets/ec2-key -o StrictHostKeyChecking=accept-new
   ubuntu@<ip>`. It is terminated at the time the answer gives: copy results
   back before then, and say in your report when you are done with it.

No kbuild section in your CLAUDE.md means this fleet has no build host, so 1
and 2 are not open to you, and a sandbox has neither: ask.

## If you are a sandbox

`FLEET_SANDBOX=1` in your environment (and `fleetctl whoami` saying scope
`sandbox`) means you are a sandbox lent to a guest. You have no GitHub access
and none of the operator's code or accounts. Code comes in with `fleetctl sync`
from the person who briefs you, into `/work/code`. Leave results there (or in
`/work/out`) and tell them the exact `fleetctl sync <you>:/work/... ./` to fetch
them. Of this reference only `whoami`, `tell` and `ask` are yours, and only to
whoever gave you your current task: fleetd refuses everything else (status,
peers, memory, the gateway, secrets, files), so do not try them.

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

## Worktrees (wt)

Every worktree you work in is worktrunk's (`wt`; its own skill, `worktrunk`,
has the whole CLI). The fleet's config (`/etc/xdg/worktrunk/config.toml`)
says where worktrees go, locks each one as it is made, and refuses merges.
The repository is the shared code's: pass it with `-C`.

    # a task's worktree: a new branch from the default branch, then work in it
    p=$(wt -C <code>/<repo> switch --create fleet/<agent>/<topic> --no-cd --format=json | jq -er .path) && cd "$p"
    #   … from the newest main:  git -C <code>/<repo> fetch origin main, then add --base origin/main
    #   … for a kbuild job (the build host cannot see /work):  add --path <code>/.worktrees/<agent>/<topic>

    wt -C <code>/<repo> list                                  # every worktree of the repository, everyone's
    wt -C <code>/<repo> list --format=json                    # the same, for jq

- `/work/wt/<repo>.<agent>-<topic>` is where a worktree goes. It lives on your
  own volume, which your slots share. The ones in `wt list` whose path is
  missing here are other agents', in their own containers.
- Leave your worktrees when the task is done: fleetd removes the ones with
  nothing to lose (clean, every commit on a remote) when your session is
  archived, and deletes their branch once it is merged. To remove one sooner:
  `git -C <path> worktree unlock <path> && wt -C <code>/<repo> remove --foreground <branch>`.
  wt keeps the branch if it is not merged.
- Never `wt merge` (the config refuses it: you push and report), never remove
  or `wt step prune` worktrees you did not make (the config refuses that too),
  and never `--no-hooks` around either.
- A repository's own hooks (`.config/wt.toml`, run on create or remove) need
  approval the first time, and here nobody can be asked: read them, then rerun
  with `--yes`.

## GitHub

`gh` and `git` pick the account from the repository: the fleet's by default,
the owner's own account for the owners listed in your CLAUDE.md. No other
account is available. Never set `GH_TOKEN` yourself or print a token. Never
push to `main`; never rebase or force-push a branch once you have reported it.

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
| `wt`: `Branch … already exists` | the branch was made before | `wt switch` it without `--create` |
| `wt`: `Cannot prompt for approval` | the repository's own hooks are new to you | read `.config/wt.toml`, then rerun with `--yes` |
| `wt`: `is not one of …'s worktrees` | a removal of someone else's worktree | leave it |
| `wt`: `worktree is locked` | your worktree, locked when made | unlock it first (Worktrees, above) |
