#!/usr/bin/env bash
# Scene 1 — PR guardian: a PR leaking an AWS key is blocked; a clean PR is approved and merged.
set -euo pipefail
cd "$(dirname "$0")/.."
. demo/lib.sh

say "PR #A: an S3 receipt uploader with the AWS key hard-coded"
BAD=$(demo/act.sh leak-pr | jq -r .pr)
echo "opened PR #$BAD"
for _ in $(seq 1 60); do
  [ "$(pr_field "[(.reviews//[])[]|select(.number==$BAD)]|length")" -gt 0 ] && break; sleep 1
done
say "KARMAX review of #$BAD"
pr_field ".reviews[]|select(.number==$BAD)|{event, body, inline_comments:(.comments|length)}"

say "PR #B: clean, conventional, with tests"
GOOD=$(demo/act.sh clean-pr | jq -r .pr)
echo "opened PR #$GOOD"
for _ in $(seq 1 60); do
  [ "$(pr_field "[(.reviews//[])[]|select(.number==$GOOD)]|length")" -gt 0 ] && break; sleep 1
done
say "CI goes green on #$GOOD"
demo/act.sh ci "$GOOD" success >/dev/null
for _ in $(seq 1 60); do
  [ "$(pr_field "[.prs[]|select(.number==$GOOD and .merged)]|length")" -gt 0 ] && break; sleep 1
done
say "Result (fake GitHub state)"
summary
