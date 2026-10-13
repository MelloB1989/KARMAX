#!/usr/bin/env bash
# Scene 3 — repo factory: ask for a repo in chat, watch it appear compliant.
# Env overrides: FAKEGH KARMAX_URL CHAT_URL ORG REPO TIMEOUT.  Flag: --auto-approve
# (answers an "approve <CODE>" prompt for you if repo_create policy is `approve`).
set -u
FAKEGH=${FAKEGH:-http://127.0.0.1:8787}
KARMAX_URL=${KARMAX_URL:-http://127.0.0.1:9090}
CHAT_URL=${CHAT_URL:-http://127.0.0.1:9393}
ORG=${ORG:-acme}; REPO=${REPO:-ledger-svc}; TIMEOUT=${TIMEOUT:-60}
AUTO=0; [ "${1:-}" = "--auto-approve" ] && AUTO=1
CMD="create repo $REPO from go: double-entry ledger"
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# chat_send TEXT — via the chat page's server, else straight into KARMAX.
chat_send() {
  local body; body=$(jq -nc --arg t "$1" '{text:$t,user:"Maya",channel:"eng"}')
  curl -fsS -m 10 -XPOST "$CHAT_URL/send" -H 'content-type: application/json' -d "$body" >/dev/null 2>&1 ||
  curl -fsS -m 10 -XPOST "$KARMAX_URL/comms/chat?token=demo-token" -H 'content-type: application/json' -d "$body" >/dev/null
}
last_id() { curl -fsS -m 5 "$CHAT_URL/messages?since=0" | jq 'map(.id) | max // 0'; }
# wait_msg SINCE REGEX — print first karmax message after SINCE matching REGEX.
wait_msg() {
  local since=$1 re=$2 m
  for _ in $(seq 1 "$TIMEOUT"); do
    m=$(curl -fsS -m 5 "$CHAT_URL/messages?since=$since" 2>/dev/null |
        jq -r --argjson s "$since" --arg re "$re" '[.[]|select(.id>$s and .from=="karmax" and (.text|test($re)))][0].text // empty')
    [ -n "$m" ] && { printf '%s\n' "$m"; return 0; }
    sleep 1
  done
  return 1
}

curl -fsS -m 5 "$FAKEGH/_fake/state" >/dev/null || { echo "fake GitHub not reachable at $FAKEGH" >&2; exit 1; }
curl -fsS -m 5 "$CHAT_URL/messages?since=0" >/dev/null || { echo "chat server not reachable at $CHAT_URL" >&2; exit 1; }
if curl -fsS -m 5 "$FAKEGH/_fake/state" | jq -e --arg r "$ORG/$REPO" '.repos|index($r)' >/dev/null; then
  echo "$ORG/$REPO already exists in the fake GitHub — run demo/down.sh && demo/up.sh (or REPO=other-name) first." >&2; exit 1
fi

since=$(last_id)
say "Maya → #eng:  $CMD"
chat_send "$CMD" || { echo "could not post to chat" >&2; exit 1; }

say "KARMAX answers"
reply=$(wait_msg "$since" 'Created|Could not create|Approval needed|asked for a new repository') || { echo "no reply in ${TIMEOUT}s — see demo/logs/karmax.log" >&2; exit 2; }
echo "$reply"
if grep -q 'Approval needed' <<<"$reply"; then
  code=$(grep -o 'approve [A-Z0-9]\{5\}' <<<"$reply" | head -1 | cut -d' ' -f2)
  if [ "$AUTO" = 1 ] && [ -n "$code" ]; then
    since=$(last_id); say "--auto-approve → approve $code"; chat_send "approve $code"
    wait_msg "$since" 'Created|Could not create' || { echo "no creation message" >&2; exit 2; }
  else
    echo; echo "Policy is 'approve': reply  approve ${code:-<CODE>}  in the chat (or re-run with --auto-approve)."; exit 0
  fi
fi

say "Proof from the (fake) GitHub: $ORG/$REPO"
curl -fsS "$FAKEGH/_fake/state" | jq -r --arg r "$ORG/$REPO" '"exists:  \(.repos|index($r) != null)\ncreated: \(.created|map(select(.==$r))|join(","))"'
echo "standard files (GET contents):"
for f in README.md CODEOWNERS CLAUDE.md .github/pull_request_template.md .github/workflows/ci.yml main.go; do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$FAKEGH/repos/$ORG/$REPO/contents/$f")
  [ "$code" = 200 ] && printf '  ✓ %s\n' "$f" || printf '  · %s (absent)\n' "$f"
done
echo "branch protection: see the 'Branch protection on main' line in the chat message above"
