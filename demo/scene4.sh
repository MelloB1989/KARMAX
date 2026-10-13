#!/usr/bin/env bash
# Scene 4 — org knowledge: index every repo, then ask a cross-repo question.
# Env overrides: FAKEGH KARMAX_URL CHAT_URL ORG TIMEOUT ASK_TIMEOUT.
set -u
FAKEGH=${FAKEGH:-${FAKE_GITHUB:-http://127.0.0.1:8787}}
KARMAX_URL=${KARMAX_URL:-http://127.0.0.1:9090}
CHAT_URL=${CHAT_URL:-http://127.0.0.1:9393}
ORG=${ORG:-acme}; TIMEOUT=${TIMEOUT:-90}; ASK_TIMEOUT=${ASK_TIMEOUT:-75}
QUESTION=${QUESTION:-What does payments-api depend on?}
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
chat_send() {
  local body; body=$(jq -nc --arg t "$1" '{text:$t,user:"Maya",channel:"eng"}')
  curl -fsS -m 10 -XPOST "$CHAT_URL/send" -H 'content-type: application/json' -d "$body" >/dev/null 2>&1 ||
  curl -fsS -m 10 -XPOST "$KARMAX_URL/comms/chat?token=demo-token" -H 'content-type: application/json' -d "$body" >/dev/null
}
last_id() { curl -fsS -m 5 "$CHAT_URL/messages?since=0" | jq 'map(.id) | max // 0'; }
wait_msg() { # SINCE REGEX SECONDS
  local since=$1 re=$2 secs=$3 m
  for _ in $(seq 1 "$secs"); do
    m=$(curl -fsS -m 5 "$CHAT_URL/messages?since=$since" 2>/dev/null |
        jq -r --argjson s "$since" --arg re "$re" '[.[]|select(.id>$s and .from=="karmax" and (.text|test($re)))][0].text // empty')
    [ -n "$m" ] && { printf '%s\n' "$m"; return 0; }
    sleep 1
  done
  return 1
}
curl -fsS -m 5 "$CHAT_URL/messages?since=0" >/dev/null || { echo "chat server not reachable at $CHAT_URL" >&2; exit 1; }

since=$(last_id)
say "Maya → #eng:  index all"
chat_send "index all" || { echo "could not post to chat" >&2; exit 1; }
wait_msg "$since" 'Indexed [0-9]+ repositories' "$TIMEOUT" || { echo "indexing did not report in ${TIMEOUT}s — see demo/logs/karmax.log" >&2; exit 2; }

# Let the chat go quiet before asking. The orchestrator agent also answers "index all"
# itself, and KARMAX withholds an agent's answer when anything else reached the same
# chat target during its turn ("chat already answered this turn; withholding the final
# text", internal/agent/agent.go repliedDuringTurn) — so a stray post mid-turn would
# swallow the answer to the question.
quiet() { local n=0 prev cur; prev=$(last_id)
  for _ in $(seq 1 "${QUIET_MAX:-45}"); do sleep 1; cur=$(last_id)
    if [ "$cur" = "$prev" ]; then n=$((n+1)); [ $n -ge "${QUIET:-8}" ] && return 0; else n=0; prev=$cur; fi
  done; }
quiet
since=$(last_id)
say "Maya → #eng:  $QUESTION"
chat_send "$QUESTION"
# an answer is any karmax message that is not one of the office's own status posts
if ans=$(wait_msg "$since" '^[^🚨🟡📦📚🛠👍🛑ℹ❌✅🔔]' "$ASK_TIMEOUT"); then
  echo "$ans"
else
  # No model auth: free-form questions go to the orchestrator agent, which needs one.
  echo "(no model answer in ${ASK_TIMEOUT}s — harness not authenticated, or the answer was withheld: grep 'withholding' demo/logs/karmax.log)" >&2
  say "Fallback: read the card straight from the store"
  since=$(last_id); chat_send "index payments-api"
  wait_msg "$since" 'Indexed' 30 || { echo "no card either — see demo/logs/karmax.log" >&2; exit 2; }
fi
