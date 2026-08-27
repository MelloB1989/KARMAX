# Sandbox image

A container with git, `gh`, and Claude Code, given one task and left to
produce a pull request. It runs headless — no interactive login, no TTY.

## Build

```sh
docker build -t karmax-sandbox -f packaging/sandbox/Dockerfile packaging/sandbox
```

`karmax.yaml`'s sandbox config should point `image:` at whatever you tag it
(push it somewhere the daemon's Docker host can pull from if that host isn't
where you built it).

## Env vars the container needs

Set by the `sandbox` recipe step / `Sandbox` Kit call via `Spec.Env` — the
Docker driver (`internal/sandbox/docker.go`) writes them to a 0600
`--env-file` rather than passing them as `docker run` arguments, so they never
show up in `docker inspect` or in the host's process list.

| Var | Required | What |
|---|---|---|
| `TASK` | yes | The task description, in prose. Overridden by `/work/task.json`'s `.task`/`.description` field if that file is present — that's how the daemon hands over something longer than fits comfortably in an env var. |
| `REPO` | yes | `owner/name` to clone. |
| `BASE_BRANCH` | yes | Branch to clone and open the PR against. |
| `GITHUB_TOKEN` | yes | A short-lived GitHub App installation token, scoped to `REPO`. Treat it as a credential: it's never logged, never put in argv, and `.git/config` never sees it (auth rides an `http.extraHeader`, not the clone URL). |
| `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` | one of the two | How Claude Code authenticates. See below. |
| `CLAUDE_PERMISSION_MODE` | no | Defaults to `acceptEdits`. See the permission-mode note below. |
| `WORK_BRANCH` | no | Defaults to `karmax/sandbox-<utc-timestamp>-<pid>`. Set it if you want a deterministic or case-linked branch name. |

## Getting `CLAUDE_CODE_OAUTH_TOKEN`

On a machine where you're already logged into Claude Code with the
subscription you want the sandbox billed against, run:

```sh
claude setup-token
```

That mints a long-lived token — paste it into KARMAX's config/secret store as
`CLAUDE_CODE_OAUTH_TOKEN`. There's no way to run this non-interactively; it's
a one-time setup step per seat.

**Seat caveat:** a `setup-token` is bound to *one* subscription seat. If
sandboxes are launched concurrently, or an org wants this running under
something other than a personal account, use a dedicated bot account with its
own Claude subscription seat — don't point every sandbox at one person's
token. The alternative is to skip `setup-token` entirely and set
`ANTHROPIC_API_KEY` instead, which bills per-token through the API rather than
through a subscription seat and has no seat-contention problem at all.

## Permission mode

`entrypoint.sh` runs `claude -p "$TASK" --permission-mode acceptEdits`.
`acceptEdits` auto-accepts file edits without needing a TTY to prompt on, but
it still gates non-edit tool calls (Bash) — with no TTY to answer an
unanswered gate, those calls fail rather than hang, so a task that needs to
run shell commands (installing deps, running tests) unattended may need more.
Set `CLAUDE_PERMISSION_MODE=bypassPermissions` (the `--permission-mode`
equivalent of `--dangerously-skip-permissions`) to allow that — it's a
reasonable choice specifically *because* this container is single-tenant and
thrown away after one run, but it does mean anything the task's prompt or the
repo's own content can talk Claude into runs with no gate at all. Decide that
per deployment, not by changing the default here.

## What it does

1. Reads `TASK`, `REPO`, `BASE_BRANCH` (and `/work/task.json` if mounted).
2. Clones `REPO` at `BASE_BRANCH` using `GITHUB_TOKEN`.
3. Creates `WORK_BRANCH`.
4. Runs `claude -p` on the task.
5. Commits whatever changed, pushes `WORK_BRANCH`, opens a PR with `gh pr
   create` (`gh` authenticates from `GH_TOKEN`, set from `GITHUB_TOKEN`).
6. Exits non-zero with a message naming the failing step if anything above
   fails — including "claude made no changes" if the task produced nothing to
   commit.

## Local dry run

```sh
docker run --rm \
  -e TASK="describe the bug in README.md and fix the typo" \
  -e REPO="you/some-repo" \
  -e BASE_BRANCH="main" \
  -e GITHUB_TOKEN="ghs_..." \
  -e CLAUDE_CODE_OAUTH_TOKEN="..." \
  karmax-sandbox
```

## Driver-side config (not consumed inside the container)

The Go driver that launches this image (`internal/sandbox/docker.go`) reads
two env vars on the **host** running the KARMAX daemon, not inside the
container:

| Var | Default | What |
|---|---|---|
| `KARMAX_SANDBOX_MEMORY` | `2g` | `docker run --memory` |
| `KARMAX_SANDBOX_CPUS` | `2` | `docker run --cpus` |
