#!/usr/bin/env bash
# Clone, branch, run Claude Code on the task, commit, push, open a PR. No
# `set -x` anywhere in this file — every secret here (GITHUB_TOKEN,
# CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY) lives only in env, and xtrace
# would echo them straight into the container's logs.
set -euo pipefail
trap 'echo "==> sandbox: failed at line ${LINENO}" >&2' ERR

: "${TASK:?TASK is required}"
: "${REPO:?REPO is required}"
: "${BASE_BRANCH:?BASE_BRANCH is required}"
: "${GITHUB_TOKEN:?GITHUB_TOKEN is required}"
if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "==> sandbox: need CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY to authenticate Claude Code" >&2
  exit 1
fi

TASK_DESC="$TASK"
if [ -f /work/task.json ] && command -v jq >/dev/null 2>&1; then
  # /work/task.json is how the daemon hands over a description too long to
  # sit comfortably in an env var; TASK stays as the fallback.
  from_file="$(jq -r '.task // .description // empty' /work/task.json 2>/dev/null || true)"
  if [ -n "$from_file" ] && [ "$from_file" != "null" ]; then
    TASK_DESC="$from_file"
  fi
fi

WORK_DIR=/work/repo
WORK_BRANCH="${WORK_BRANCH:-karmax/sandbox-$(date -u +%Y%m%d%H%M%S)-$$}"
# Basic-auth header built once and passed per-command via -c, so the token
# never lands in .git/config or a `git remote -v` a later step could echo.
AUTH_HEADER="Authorization: basic $(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64 -w0)"

echo "==> sandbox: cloning ${REPO}@${BASE_BRANCH}"
git -c http.extraHeader="$AUTH_HEADER" clone --branch "$BASE_BRANCH" --single-branch \
  "https://github.com/${REPO}.git" "$WORK_DIR"
cd "$WORK_DIR"
git config user.name "karmax-sandbox"
git config user.email "sandbox@karmax.local"

echo "==> sandbox: branching ${WORK_BRANCH}"
git checkout -b "$WORK_BRANCH"

echo "==> sandbox: running Claude Code"
# --permission-mode acceptEdits auto-accepts file edits without gating on a
# TTY that doesn't exist here. It still gates non-edit tool calls (Bash) —
# if the task needs those unattended too, set CLAUDE_PERMISSION_MODE=
# bypassPermissions (== --dangerously-skip-permissions); safe only because
# this container is single-tenant and disposable, so document that choice
# where you set it, not here.
if ! claude -p "$TASK_DESC" \
    --permission-mode "${CLAUDE_PERMISSION_MODE:-acceptEdits}" \
    --output-format text \
    2>&1 | tee /work/claude.log; then
  echo "==> sandbox: claude failed" >&2
  exit 1
fi

echo "==> sandbox: committing"
git add -A
if git diff --cached --quiet; then
  echo "==> sandbox: claude made no changes — nothing to commit" >&2
  exit 1
fi
git commit -m "$(printf '%s' "$TASK_DESC" | head -n1 | cut -c1-200)"

echo "==> sandbox: pushing ${WORK_BRANCH}"
git -c http.extraHeader="$AUTH_HEADER" push -u origin "$WORK_BRANCH"

echo "==> sandbox: opening PR"
export GH_TOKEN="$GITHUB_TOKEN"
pr_title="$(printf '%s' "$TASK_DESC" | head -n1 | cut -c1-120)"
pr_body="$(printf 'Opened automatically by a KARMAX sandbox run.\n\nTask:\n\n%s\n' "$TASK_DESC")"
gh pr create --repo "$REPO" --base "$BASE_BRANCH" --head "$WORK_BRANCH" \
  --title "$pr_title" --body "$pr_body"

echo "==> sandbox: done"
