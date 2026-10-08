#!/usr/bin/env bash
# Phase 0 spike for docs/AGENT-FLEET.md (revision 2). Run on Kali, ~15 minutes:
#   packaging/fleet/spike.sh
#
#   S1  sessions in SEPARATE containers sharing one PID namespace, on two
#       subscriptions, message each other natively — and a control container
#       outside the namespace can't
#   S2  a -p stream-json session (how KARMAX drives the orchestrator) that is
#       messaged while idle: does it start a turn by itself, and what comes out?
#   S3  typing into an agent's tmux pane works as a prompt (fleetctl prompt)
#   S4  the StopFailure hook fires on a bad token; prints the payload
#   S5  through `docker exec -i`: does closing stdin end claude inside the
#       container, and does killing the docker client leave it orphaned?
#   S6  the ledger taps: the status line reports the subscription's 5h/7d
#       windows inside a container on a setup-token, and hooks write events
#
# Needs Docker and CLAUDE_CODE_OAUTH_TOKEN (claude setup-token). Set
# CLAUDE_CODE_OAUTH_TOKEN_2 to a second subscription's token to run b on its own
# account, as the fleet will. Spends a handful of haiku turns.
# `./spike.sh clean` removes everything it made.
set -euo pipefail

P=fleet-spike
cleanup() {
  docker rm -f "$P-anchor" "$P-a" "$P-b" "$P-c" >/dev/null 2>&1 || true
  docker volume rm "$P-peers" "$P-tmp" >/dev/null 2>&1 || true
}
if [ "${1:-}" = clean ]; then cleanup; exit 0; fi

for t in docker jq; do command -v "$t" >/dev/null || { echo "spike needs $t on the host (sudo apt install $t)"; exit 1; }; done
: "${CLAUDE_CODE_OAUTH_TOKEN:?export CLAUDE_CODE_OAUTH_TOKEN (run: claude setup-token)}"
VER="${CLAUDE_VERSION:-$(claude --version 2>/dev/null | awk '{print $1}' || true)}"
VER="${VER:-latest}"
WORK="$(mktemp -d)"
REPO=/home/node/code/repo
cleanup

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
pass() { printf '\033[32mPASS\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; }

say "building image with Claude Code $VER"
mkdir -p "$WORK/ctx" "$WORK/repo"
cat >"$WORK/ctx/Dockerfile" <<'EOF'
FROM node:22-bookworm-slim
ARG CLAUDE_VERSION=latest
RUN apt-get update && apt-get install -y --no-install-recommends git tini tmux procps jq ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION}
ENV DISABLE_AUTOUPDATER=1 TERM=xterm-256color
USER node
RUN mkdir -p /home/node/.claude/sessions /home/node/.claude/fleet /home/node/code/repo \
 && echo '{"hasCompletedOnboarding":true,"projects":{"/home/node/code/repo":{"hasTrustDialogAccepted":true}}}' > /home/node/.claude.json
COPY --chown=node:node settings.json /home/node/.claude/settings.json
COPY --chown=node:node --chmod=755 status.sh event.sh /home/node/.claude/fleet/
WORKDIR /home/node/code/repo
ENTRYPOINT ["/usr/bin/tini","--"]
CMD ["sleep","infinity"]
EOF
# inbound accept is explicit so the bypass-vs-prompting default never holds a
# message; SendMessage/ListAgents are pre-allowed so no prompt blocks a pane;
# StopFailure keeps every payload for S4; the status line and the other hooks
# are the ledger taps S6 checks.
cat >"$WORK/ctx/settings.json" <<'JSON'
{
  "permissions": {"defaultMode": "acceptEdits", "allow": ["SendMessage", "ListAgents"]},
  "crossSessionInbound": "accept",
  "dialogExpiry": "never",
  "statusLine": {"type": "command", "command": "/home/node/.claude/fleet/status.sh", "refreshInterval": 30},
  "hooks": {
    "StopFailure": [{"matcher": "*", "hooks": [{"type": "command", "command": "mkdir -p ~/.claude/fleet/stopfail && cat > ~/.claude/fleet/stopfail/$(date +%s%N).json"}]}],
    "SessionStart": [{"hooks": [{"type": "command", "command": "/home/node/.claude/fleet/event.sh"}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "/home/node/.claude/fleet/event.sh"}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "/home/node/.claude/fleet/event.sh"}]}]
  }
}
JSON
# status.sh keeps the latest snapshot for fleetd and prints a short line.
cat >"$WORK/ctx/status.sh" <<'SH'
#!/bin/sh
in="$(cat)"; d="$HOME/.claude/fleet"
printf '%s\n' "$in" >"$d/status.json.tmp" && mv "$d/status.json.tmp" "$d/status.json"
printf '%s\n' "$in" | jq -r '"\(.model.display_name // "?") ctx \(.context_window.used_percentage // 0)% 5h \(.rate_limits.five_hour.used_percentage // "-")%"'
SH
# event.sh appends one JSON line per hook event.
cat >"$WORK/ctx/event.sh" <<'SH'
#!/bin/sh
jq -c '{t: (now|floor), event: .hook_event_name, session: .session_id}' >>"$HOME/.claude/fleet/events.jsonl"
SH
docker build -q -t "$P" --build-arg CLAUDE_VERSION="$VER" "$WORK/ctx" >/dev/null

git -C "$WORK/repo" init -q && chmod -R a+rwX "$WORK/repo"
# One 0600 env file per container: the fleet's shape, and no tokens in argv.
envfile() { (umask 077; printf 'CLAUDE_CODE_OAUTH_TOKEN=%s\n' "$1" >"$WORK/$2.env"); }
envfile "$CLAUDE_CODE_OAUTH_TOKEN" acct1
envfile "${CLAUDE_CODE_OAUTH_TOKEN_2:-$CLAUDE_CODE_OAUTH_TOKEN}" acct2
if [ -n "${CLAUDE_CODE_OAUTH_TOKEN_2:-}" ]; then echo "b runs on the second subscription"
else echo "no CLAUDE_CODE_OAUTH_TOKEN_2: a and b share one subscription (cross-account untested)"; fi

docker volume create "$P-peers" >/dev/null
docker volume create "$P-tmp" >/dev/null
docker run -d --name "$P-anchor" "$P" >/dev/null
run_box() { # name, account, extra docker args...
  local n="$1" acct="$2"; shift 2
  docker run -d --name "$P-$n" "$@" --env-file "$WORK/$acct.env" \
    -v "$P-peers:/home/node/.claude/sessions" -v "$P-tmp:/tmp" \
    -v "$WORK/repo:$REPO" "$P" >/dev/null
}
run_box a acct1 --pid "container:$P-anchor"
run_box b acct2 --pid "container:$P-anchor"
run_box c acct1   # control: same volumes, own PID namespace

ex() { docker exec -w "$REPO" "$@"; }
received() { grep -q "$1" "$WORK/repo/RECEIVED.txt" 2>/dev/null; }
wait_for() { for _ in $(seq 1 45); do received "$1" && return 0; sleep 2; done; return 1; }
type_into() { # container, session, text — what `fleetctl prompt` will do
  docker exec "$1" tmux send-keys -t "$2" -l "$3"
  sleep 1; docker exec "$1" tmux send-keys -t "$2" Enter
}
status_of() { ex "$1" claude agents --json | jq -r --arg n "$2" '.[] | select(.name==$n) | .status' | head -1; }
wait_idle() { for _ in $(seq 1 45); do [ "$(status_of "$1" "$2")" = idle ] && return 0; sleep 2; done; return 1; }

say "agent: interactive claude in tmux in b, named worker-b"
docker exec -w "$REPO" "$P-b" tmux new-session -d -s main -x 200 -y 50 "claude --name worker-b"
sleep 10
type_into "$P-b" main "You are worker-b. Other sessions will message you. Whenever one arrives, add its exact text as a new line to RECEIVED.txt in $REPO using your file tools. Also do that now with the line typed-ok."
say "S3: typing into the pane acts as a prompt"
if wait_for typed-ok; then pass "S3 tmux send-keys drives the live session"
else fail "S3 — pane:"; docker exec "$P-b" tmux capture-pane -p -t main | tail -n 25; fi
wait_idle "$P-b" worker-b || echo "worker-b never reported idle (status: $(status_of "$P-b" worker-b))"

say "registry as seen from a"
ex "$P-a" sh -c 'for f in ~/.claude/sessions/*.json; do jq -c "{name,kind,status,pidDomain,messagingSocketPath}" "$f"; done' || true

send_from() { # container, token
  ex "$1" claude -p --model haiku --dangerously-skip-permissions \
    "Call ListAgents and print its output. Then use SendMessage to send the session named worker-b exactly this text: $2. Then stop." | tail -n 15 || true
}
say "S1: a -> worker-b (shared PID namespace, different subscription)"
send_from "$P-a" ping-from-a
if wait_for ping-from-a; then pass "S1 cross-container messaging works in a shared PID namespace"
else fail "S1 not delivered — fall back to one container per host"; fi

say "S1 control: c -> worker-b (own PID namespace, same volumes)"
send_from "$P-c" ping-from-c
if wait_for ping-from-c; then echo "control ALSO delivered — the PID namespace is not the gate (note this)"
else pass "control failed as expected — the shared namespace is what makes S1 work"; fi

# S2 and S5 drive claude exactly as KARMAX's harness will: a host-side
# `docker exec -i` whose stdin we hold and whose stdout we read.
user_line() { jq -nc --arg t "$1" '{type:"user",message:{role:"user",content:$t}}'; }
results() { local n; n="$(grep -c '"type":"result"' "$WORK/brain.out" 2>/dev/null)" || true; echo "${n:-0}"; }
brain_alive() { docker exec "$P-a" pgrep -f -- "--name brain" >/dev/null; }

say "S2: stream-json session 'brain' in a, messaged while idle"
coproc BRAIN { docker exec -i -w "$REPO" "$P-a" claude -p --input-format stream-json \
  --output-format stream-json --verbose --name brain --model haiku \
  --dangerously-skip-permissions >"$WORK/brain.out" 2>"$WORK/brain.err"; }
user_line "Reply with the word ok." >&"${BRAIN[1]}"
for _ in $(seq 1 30); do [ "$(results)" -ge 1 ] && break; sleep 2; done
echo "first turn results: $(results)"
type_into "$P-b" main "Use SendMessage to send the session named brain exactly this text: pong-from-b. Do not write to RECEIVED.txt for this."
for _ in $(seq 1 45); do [ "$(results)" -ge 2 ] && break; sleep 2; done
if [ "$(results)" -ge 2 ]; then
  pass "S2 an idle stream-json session starts a turn on an inbound message — KARMAX must handle unsolicited turns"
else
  echo "S2 no second turn — the message may be held or queued until the next stdin write"
fi
echo "event shape after the first result (what the supervisor must parse):"
awk 'f; /"type":"result"/{f=1}' "$WORK/brain.out" | jq -c '{type, subtype, text: (.message.content[0].text? // .result? // null)}' 2>/dev/null | head -20 || true

say "S5a: close stdin of the docker exec client"
fd="${BRAIN[1]}"; eval "exec ${fd}>&-"
sleep 8
if brain_alive; then fail "S5a claude still running after EOF — Close must also kill inside the container"
else pass "S5a EOF ends claude inside the container"; fi

say "S5b: kill the docker exec client instead"
coproc BRAIN2 { docker exec -i -w "$REPO" "$P-a" claude -p --input-format stream-json \
  --output-format stream-json --verbose --name brain --model haiku \
  --dangerously-skip-permissions >/dev/null 2>&1; }
sleep 5
kill -INT "$BRAIN2_PID" 2>/dev/null || true
sleep 8
if brain_alive; then fail "S5b killing the client orphans claude inside — Close must send EOF first (planned)"
else pass "S5b killing the client also ends it"; fi
docker exec "$P-a" pkill -f -- "--name brain" 2>/dev/null || true

say "S4: StopFailure on a bad token"
docker exec -w "$REPO" "$P-b" tmux new-session -d -s probe -x 200 -y 50 \
  "env CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-not-a-real-token claude --name auth-probe 'Reply with hi.'"
sleep 25
if docker exec "$P-b" sh -c 'ls ~/.claude/fleet/stopfail/*.json' >/dev/null 2>&1; then
  pass "S4 StopFailure fired — payloads:"
  docker exec "$P-b" sh -c 'for f in ~/.claude/fleet/stopfail/*.json; do cat "$f"; echo; done'
else
  fail "S4 no StopFailure payload — pane:"; docker exec "$P-b" tmux capture-pane -p -t probe | tail -n 20
fi

say "S6: ledger taps in worker-b"
sleep 35   # one status-line refresh interval
snap="$(docker exec "$P-b" cat /home/node/.claude/fleet/status.json 2>/dev/null || true)"
if [ -n "$snap" ] && printf '%s' "$snap" | jq -e '.rate_limits.five_hour.used_percentage != null' >/dev/null 2>&1; then
  pass "S6 status line reports the subscription's windows:"
  printf '%s' "$snap" | jq -c '{session_id, model: .model.id, ctx: .context_window.used_percentage, cost: .cost.total_cost_usd, rate_limits}'
elif [ -n "$snap" ]; then
  fail "S6 status line runs but carries no rate_limits — quota falls back to StopFailure only"
  printf '%s' "$snap" | jq -c 'keys'
else
  fail "S6 no status line snapshot — is the status line rendered in a detached tmux pane?"
fi
echo "hook events:"; docker exec "$P-b" tail -n 8 /home/node/.claude/fleet/events.jsonl 2>/dev/null || echo "(none)"

say "results"
echo "RECEIVED.txt:"; cat "$WORK/repo/RECEIVED.txt" 2>/dev/null || echo "(none)"
echo
echo "Socket dir to share in production = the directory of messagingSocketPath above."
echo "Pin this version in packaging/fleet/Dockerfile: $VER"
echo "Attach to the agent yourself: docker exec -it $P-b tmux attach -t main"
echo "Done poking? $0 clean"
