#!/usr/bin/env bash
# Scene 1 — PR guardian: a PR leaking an AWS key is blocked; a clean PR is approved and merged.
set -euo pipefail
cd "$(dirname "$0")/.."
. demo/lib.sh

say "PR #A: adds a Stripe/AWS client — leaks a key, sloppy title"
BAD=$(open_pr '{"repo":"acme/payments-api","title":"added aws uploader","body":"quick thing","author":"dev1",
  "files":[{"filename":"uploader.go","patch":"@@ -0,0 +1,4 @@\n+package main\n+\n+const awsKey = \"AKIAIOSFODNN7EXAMPLE\"\n+const awsSecret = \"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\""}]}')
echo "opened PR #$BAD"
for _ in $(seq 1 60); do
  [ "$(pr_field "[(.reviews//[])[]|select(.number==$BAD)]|length")" -gt 0 ] && break; sleep 1
done
say "KARMAX review of #$BAD"
pr_field ".reviews[]|select(.number==$BAD)|{event, body, inline_comments:(.comments|length)}"

say "PR #B: clean, conventional, with tests"
patch_of() { { printf '@@ -0,0 +1,%d @@\n' "$(wc -l <"$1")"; sed 's/^/+/' "$1"; }; }
GOOD=$(jq -n --rawfile a <(patch_of demo/prs/refunds.go.txt) --rawfile t <(patch_of demo/prs/refunds_test.go.txt) '{
  repo:"acme/payments-api", title:"feat: add validated refund helper", author:"dev2",
  body:"Adds Refund(ctx, store, id, amount): validates the id and amount (integer minor units), rejects over-refunds with typed errors. Table-driven tests cover each error.",
  files:[{filename:"refunds.go",status:"added",patch:$a},{filename:"refunds_test.go",status:"added",patch:$t}]}' \
  | curl -fsS -XPOST "$GH/_fake/pulls" -d @- | jq -r .number)
echo "opened PR #$GOOD"
for _ in $(seq 1 60); do
  [ "$(pr_field "[(.reviews//[])[]|select(.number==$GOOD)]|length")" -gt 0 ] && break; sleep 1
done
say "CI goes green on #$GOOD"
set_ci "$GOOD" success
for _ in $(seq 1 60); do
  [ "$(pr_field "[.prs[]|select(.number==$GOOD and .merged)]|length")" -gt 0 ] && break; sleep 1
done
say "Result (fake GitHub state)"
summary
