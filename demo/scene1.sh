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
  [ "$(pr_field "[.reviews[]|select(.number==$BAD)]|length")" -gt 0 ] && break; sleep 1
done
say "KARMAX review of #$BAD"
pr_field ".reviews[]|select(.number==$BAD)|{event, body, inline_comments:(.comments|length)}"

say "PR #B: clean, conventional, with tests"
GOOD=$(open_pr '{"repo":"acme/payments-api","title":"feat: add refunds endpoint","body":"Adds POST /charges/{id}/refund with validation. Tested with refunds_test.go.","author":"dev2",
  "files":[{"filename":"refunds.go","patch":"@@ -0,0 +1,3 @@\n+package payments\n+\n+func Refund(id string) error { return nil }"},
           {"filename":"refunds_test.go","patch":"@@ -0,0 +1,5 @@\n+package payments\n+\n+import \"testing\"\n+\n+func TestRefund(t *testing.T) { _ = Refund(\"ch_1\") }"}]}')
echo "opened PR #$GOOD"
for _ in $(seq 1 60); do
  [ "$(pr_field "[.reviews[]|select(.number==$GOOD)]|length")" -gt 0 ] && break; sleep 1
done
say "CI goes green on #$GOOD"
set_ci "$GOOD" success
for _ in $(seq 1 60); do
  [ "$(pr_field "[.prs[]|select(.number==$GOOD and .merged)]|length")" -gt 0 ] && break; sleep 1
done
say "Result (fake GitHub state)"
summary
