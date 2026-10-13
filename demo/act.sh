#!/usr/bin/env bash
# One quick, non-blocking demo step. Prints a single JSON line and exits 0, or
# {"ok":false,"error":"…"} and exits 1. The console's buttons call this.
#
#   act.sh leak-pr              open the PR that hard-codes an AWS key          -> "pr":N
#   act.sh clean-pr             open a clean feature PR with tests              -> "pr":N
#   act.sh ci <pr> success|failure
#   act.sh break | heal         payments fault on / off
#   act.sh say "<text>" [user]  post into chat as a person (default Maya)
#   act.sh seed                 one-time org setup (more repos, one non-compliant)
#   act.sh reset                detached `down.sh && up.sh`; returns at once
#
# Ports/URLs come from env (FAKE_GITHUB CHAT_URL PAYMENTS_URL KARMAX_URL) like lib.sh.
set -uo pipefail
cd "$(dirname "$0")/.."
. demo/lib.sh
ORG=${ORG:-acme}

out() { jq -cn --arg action "$ACTION" "{ok:true,action:\$action} + $1" "${@:2}"; }
fail() { jq -cn --arg e "$*" --arg action "${ACTION:-}" '{ok:false,action:$action,error:$e}'; exit 1; }
post() { curl -fsS -m 10 -XPOST -H 'Content-Type: application/json' "$1" -d "$2"; } # url body
# patch_of FILE: a whole-file "added" hunk
patch_of() { printf '@@ -0,0 +1,%d @@\n' "$(wc -l <"$1")"; sed 's/^/+/' "$1"; }

ACTION=${1:-}; shift || true
command -v jq >/dev/null || { echo '{"ok":false,"error":"jq not installed"}'; exit 1; }

case "$ACTION" in
leak-pr)
  body=$(jq -n --rawfile p <(patch_of demo/prs/leak-s3-uploader.go.txt) --arg repo "$ORG/payments-api" '{
    repo:$repo, title:"Add S3 receipt uploader", author:"dev1",
    body:"Stores receipt PDFs in S3 so support can fetch them. Quick version, will tidy later.",
    files:[{filename:"receipts/uploader.go",status:"added",patch:$p}]}')
  n=$(post "$GH/_fake/pulls" "$body" | jq -r .number) || fail "fake GitHub not reachable at $GH"
  out '{pr:($n|tonumber),repo:$repo}' --arg n "$n" --arg repo "$ORG/payments-api" ;;

clean-pr)
  body=$(jq -n --rawfile a <(patch_of demo/prs/refunds.go.txt) --rawfile t <(patch_of demo/prs/refunds_test.go.txt) --arg repo "$ORG/payments-api" '{
    repo:$repo, title:"feat: add validated refund helper", author:"dev2",
    body:"Adds Refund(ctx, store, id, amount): validates the id and amount (integer minor units), rejects over-refunds with typed errors. Table-driven tests cover each error.",
    files:[{filename:"refunds.go",status:"added",patch:$a},{filename:"refunds_test.go",status:"added",patch:$t}]}')
  n=$(post "$GH/_fake/pulls" "$body" | jq -r .number) || fail "fake GitHub not reachable at $GH"
  out '{pr:($n|tonumber),repo:$repo}' --arg n "$n" --arg repo "$ORG/payments-api" ;;

ci)
  pr=${1:-}; state=${2:-}
  [[ "$pr" =~ ^[0-9]+$ ]] || fail "usage: act.sh ci <pr-number> success|failure"
  case "$state" in success|failure|pending) ;; *) fail "ci state must be success, failure or pending" ;; esac
  post "$GH/_fake/ci" "{\"repo\":\"${REPO_FULL:-$ORG/payments-api}\",\"number\":$pr,\"state\":\"$state\"}" >/dev/null || fail "fake GitHub not reachable at $GH"
  out '{pr:($pr|tonumber),state:$s}' --arg pr "$pr" --arg s "$state" ;;

break)
  post "$PAY/fault/on" '{}' >/dev/null || fail "payments not reachable at $PAY"
  # What a customer would do: refund a guest-checkout charge. Panics in the handler while the fault is on.
  code=$(curl -s -m 5 -o /dev/null -w '%{http_code}' -XPOST "$PAY/charges/ch_1004/refund")
  out '{fault:true,refund_status:($c|tonumber)}' --arg c "$code" ;;

heal)
  post "$PAY/fault/off" '{}' >/dev/null || fail "payments not reachable at $PAY"
  out '{fault:false}' ;;

say)
  text=${1:-}; user=${2:-Maya}
  [ -n "$text" ] || fail 'usage: act.sh say "<text>" [user]'
  r=$(curl -sS -m 10 -XPOST -H 'Content-Type: application/json' "$CHAT/send" -d "$(jq -nc --arg t "$text" --arg u "$user" '{text:$t,user:$u,channel:"eng"}')" 2>&1)
  [ "$(printf %s "$r" | jq -r '.ok // false' 2>/dev/null)" = true ] || fail "chat send failed: $(e=$(printf %s "$r" | jq -r '.error' 2>/dev/null); printf %s "${e:-$r}" | head -c 300)"
  out '{user:$u,text:$t}' --arg u "$user" --arg t "$text" ;;

seed)
  # Demo-side only: the fake GitHub's /_fake/repos control endpoint (AddRepo; re-seeding resets these repos).
  add() { # name description files-json
    post "$GH/_fake/repos" "$(jq -nc --arg r "$ORG/$1" --arg d "$2" --argjson f "$3" '{repo:$r,description:$d,files:$f}')" >/dev/null || fail "could not create $1 (fake GitHub at $GH)"
  }
  curl -fsS -m 5 "$GH/_fake/state" >/dev/null || fail "fake GitHub not reachable at $GH"
  add web-app "Customer web app: checkout, account and receipts pages" "$(jq -nc '{
    "README.md":"# web-app\n\nThe customer-facing React + TypeScript app (Next.js 14): checkout, account, receipts.\n\n## Dependencies\n\n- **payments-api** — creates charges and fetches receipts over REST.\n- **auth-service** — login and session tokens (JWT).\n- **notifications** — sends the order confirmation emails.\n\nRun: `npm run dev` · Test: `npm test`\n",
    "CLAUDE.md":"# Rules for web-app\n\n- Never log card or token data.\n- Every page has a loading and an error state.\n- Strict TypeScript; no `any`.\n",
    "CODEOWNERS":"* @frontend-team\n",
    ".github/workflows/ci.yml":"name: ci\non: [pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: npm ci && npm test\n"}')"
  add notifications "Email and push notifications for orders and incidents" "$(jq -nc '{
    "README.md":"# notifications\n\nSends order confirmation emails and push notifications from queue events. Python 3.12, Celery, Redis, SES.\n\n## Dependencies\n\n- **payments-api** — consumes `charge.succeeded` and `charge.refunded` webhooks.\n- **auth-service** — looks up the user'"'"'s verified email address.\n- Redis (task queue) and AWS SES (delivery).\n\nNote: the repository was imported from the old monolith and has no CODEOWNERS yet.\n"}')"
  add infra "Terraform for the acme cloud: networks, databases, clusters" "$(jq -nc '{
    "README.md":"# infra\n\nTerraform modules for the acme cloud: VPC, Postgres (RDS), Redis, the Kubernetes cluster that runs every service.\n\n## Dependencies\n\n- Terraform 1.8, AWS provider.\n- Provides the Postgres used by **payments-api** and **auth-service**, and the Redis used by **auth-service** and **notifications**.\n\nApply only through CI: `terraform plan` runs on every PR.\n",
    "CODEOWNERS":"* @platform-team\n",
    ".github/workflows/ci.yml":"name: ci\non: [pull_request]\njobs:\n  plan:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: terraform init && terraform validate && terraform plan\n"}')"
  # The template ships the standard files too, so only "notifications" is out of standard.
  add template-go-service "Go service template" "$(jq -nc '{
    "README.md":"# Go service template\n\nHTTP service skeleton with health checks, structured logging and CI.\n",
    "CODEOWNERS":"* @platform-team\n",
    "main.go":"package main\n\nfunc main() {}\n",
    ".github/workflows/ci.yml":"name: ci\non: [pull_request]\njobs:\n  test:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n      - run: go test ./...\n"}')"
  # payments-api and auth-service come from fakegithub -seed; web-app names them, so "who depends on payments?" has answers.
  n=$(curl -fsS "$GH/_fake/state" | jq '.repos|length')
  out '{repos:($n|tonumber),noncompliant:["notifications"]}' --arg n "$n" ;;

reset)
  mkdir -p demo
  nohup bash -c 'sleep 1; demo/down.sh; demo/up.sh' >/tmp/karmax-demo-reset.$$.log 2>&1 </dev/null &
  disown
  out '{detached:true}' ;;

*) fail "unknown action '${ACTION}' (leak-pr clean-pr ci break heal say seed reset)" ;;
esac
