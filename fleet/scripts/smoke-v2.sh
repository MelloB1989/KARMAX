#!/bin/bash
# End-to-end smoke test of fleet v2, the way smoke.sh is of v1: the real
# fleetd and fleetctl, real tmux, smoke.sh's fake docker and claude, and
# fake gitloom, KARMAX and tailscaled (scripts/smoke-fakes.py). No Docker,
# no network. Each check prints PASS or FAIL with why; the run exits 1 if any
# failed. Features still on branches have their own section at the end,
# skipped while this build has no such command. Run from fleet/:
# scripts/smoke-v2.sh (-v also prints fleetd's log).
set -u
cd "$(dirname "$0")/.."
VERBOSE=${1:-}
R=$(mktemp -d); B=$R/fleetbin
go build -o "$B/" ./cmd/fleetd ./cmd/fleetctl || exit 1
mkdir -p "$R"/{bin,fakes,home/.claude/sessions,home/.claude/fleet,home/.claude/projects/-work,work,state}
chmod 700 "$R/state"
. scripts/smoke-lib.sh; smoke_fakes "$R"
export TMUX_TMPDIR=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/fl.XXXX") # sockets need a short path
export HOME=$R/home FLEET_WORK=$R/work PATH=$R/bin:$PATH RELAY_LOG=$R/relay.log

API=127.0.0.1:$(free_port) TAILNET=127.0.0.1:$(free_port) GITLOOM=$(free_port) KARMAX=$(free_port) OTLP=127.0.0.1:$(free_port)
FULL=smoke-full RELAY=smoke-relay CONSOLE=smoke-console GITLOOM_KEY=gl-smoke-key KARMAX_TOKEN=km-smoke-token
cat > "$R/state/fleet.yaml" <<EOF
home: $R/home
env_file: $R/env
state_dir: $R/state
listen: $API
otlp_listen: $OTLP
hosts: {kali: {exec: [$R/bin/docker]}}
orchestrator: {host: kali, token_env: CLAUDE_TOKEN_KARMAX}
agents:
  agent-01: {host: kali, token_env: CLAUDE_TOKEN_AGENT_01, models: [sonnet, haiku], slots: 1}
  agent-02: {host: kali, token_env: CLAUDE_TOKEN_AGENT_02, slots: 2}
karmax: {notify: false, api_url: "http://127.0.0.1:$KARMAX"}
memory: {url: "http://127.0.0.1:$GITLOOM", key_env: GITLOOM_KEY, namespace: operator}
tailnet: {listen: "$TAILNET", members: [ram@example.com], socket: $R/fakes/tailscaled.sock}
thresholds: {tick: 1s, question_escalate: 2s}
console: {host: kali, fleetd_url: "http://$API", token_env: FLEET_CONSOLE_TOKEN}
EOF
cat > "$R/env" <<EOF
FLEET_TOKEN=$FULL
FLEET_RELAY_TOKEN=$RELAY
FLEET_CONSOLE_TOKEN=$CONSOLE
CLAUDE_TOKEN_KARMAX=k
CLAUDE_TOKEN_AGENT_01=a1
CLAUDE_TOKEN_AGENT_02=a2
GITLOOM_KEY=$GITLOOM_KEY
KARMAX_API_TOKEN=$KARMAX_TOKEN
EOF
chmod 600 "$R/env"
export FLEET_CONFIG=$R/state/fleet.yaml

python3 scripts/smoke-fakes.py gitloom $GITLOOM "$R/fakes" $GITLOOM_KEY & P1=$!
python3 scripts/smoke-fakes.py karmax $KARMAX "$R/fakes" $KARMAX_TOKEN & P2=$!
python3 scripts/smoke-fakes.py tailscaled "$R/fakes/tailscaled.sock" ram@example.com & P3=$!

# agent-01's live session: a pane, its session file and transcript.
tmux new-session -d -s main -e FLEET_AGENT=agent-01 "exec sleep 600"; sleep 0.3
pid=$(tmux list-panes -t main -F '#{pane_pid}')
echo "{\"pid\":$pid,\"kind\":\"interactive\",\"sessionId\":\"s1\",\"name\":\"agent-01\",\"status\":\"idle\"}" > "$HOME/.claude/sessions/$pid.json"
mkdir -p "$R/work/.fleet"; echo s1 > "$R/work/.fleet/current"
echo '{"type":"user","sessionId":"s1","cwd":"/work","timestamp":"2026-10-08T10:00:00Z","message":{"role":"user","content":"hello"}}' > "$HOME/.claude/projects/-work/s1.jsonl"
echo '{"session_id":"s1","context_window":{"used_percentage":12},"rate_limits":{"five_hour":{"used_percentage":40,"resets_at":1891430000}}}' > "$HOME/.claude/fleet/status.json"

start_fleetd() { "$B/fleetd" >> "$R/fleetd.log" 2>&1 & FPID=$!; }
start_fleetd
trap 'kill $FPID $P1 $P2 $P3 2>/dev/null; tmux kill-server 2>/dev/null; rm -rf "$R" "$TMUX_TMPDIR"' EXIT
up() {
  for _ in $(seq 50); do curl -sf "http://$API/healthz" >/dev/null && return; sleep 0.1; done
  echo "FAIL fleetd did not start:"; cat "$R/fleetd.log"; exit 1
}
up

FAILS=0 PASSES=0
pass() { echo "PASS $1"; PASSES=$((PASSES + 1)); }
fail() { echo "FAIL $1: $2"; FAILS=$((FAILS + 1)); }
# check NAME WHY CMD… passes when CMD succeeds, and fails with WHY.
check() { local n=$1 why=$2; shift 2; if "$@"; then pass "$n"; else fail "$n" "$why"; fi; }
has() { grep -q -- "$2" <<<"$1"; }

# The operator: a Claude Code session of theirs on the fleet host (full token
# from fleet.yaml), whose inbox is cc-smoke.
op() { FLEET_SESSION=cc-smoke "$B/fleetctl" "$@"; }
# agent-01 as inside its container: the relay token and its own key, no
# fleet.yaml. KEY is what fleetctl render writes: identity.Key.
KEY=$(python3 -c 'import hmac,hashlib,sys; print(hmac.new(sys.argv[1].encode(), b"agent:agent-01", hashlib.sha256).hexdigest())' $FULL)
agent() { FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$RELAY FLEET_AGENT=agent-01 FLEET_AGENT_KEY=$KEY "$B/fleetctl" "$@"; }
# A member on the tailnet: no token, Tailscale (the fake) says ram@example.com.
member() { FLEET_CONFIG=$R/none FLEETD_URL=http://$TAILNET "$B/fleetctl" "$@"; }
# The console's token.
consoleTok() { FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$CONSOLE "$B/fleetctl" "$@"; }

echo "== model"
op model agent-01 opus >/dev/null
out=$(op model agent-01 --json)
check "model set" "override is not opus: $out" test "$(jq -r .override <<<"$out")" = opus
kill $FPID; wait $FPID 2>/dev/null; start_fleetd; up
out=$(op model agent-01 --json)
check "model override survives a fleetd restart" "after restart: $out" test "$(jq -r .override <<<"$out")" = opus
op model agent-01 --reset >/dev/null
out=$(op model agent-01 --json)
check "model reset" "override still set: $out" test "$(jq -r '.override // ""' <<<"$out")" = ""
out=$(op model agent-01 gpt/5 2>&1)
check "model refuses a name that is not a model" "accepted: $out" has "$out" "want sonnet, opus, haiku"

echo "== run --model, ask and answer"
out=$(op run "Find the port fleetd should listen on." --agent agent-01 --task T-ASK --model haiku --json 2>&1)
check "run --model hands agent-01 the task" "run said: $out" test "$(jq -r .agent <<<"$out" 2>/dev/null)" = agent-01
out=$(op model agent-01 --json)
check "run --model sets the task's model" "model: $out" test "$(jq -r .task <<<"$out")" = haiku
agent ask "Which port, 7878 or 7979?" --timeout 1m > "$R/ask.out" 2>&1 & ASK=$!
out=$(op wait --task T-ASK --new --as cc-smoke --timeout 20s 2>&1)
check "the question reaches the assigner's session inbox" "wait said: $out" has "$out" "7878 or 7979"
sleep 4 # question_escalate is 2s; the tick is 1s
check "an unanswered question escalates to the orchestrator" "relay log: $(tail -c 400 "$RELAY_LOG" 2>/dev/null)" grep -q "nobody has answered" "$RELAY_LOG"
out=$(op answer T-ASK "7979, the relay port" 2>&1)
check "answer unblocks the ask" "answer said: $out" has "$out" "ask returned it"
wait $ASK; rc=$?
check "the agent's ask returns the operator's answer in the same command" "exit $rc: $(cat "$R/ask.out")" grep -qx "7979, the relay port" "$R/ask.out"

echo "== tell --kind and wait --new"
agent tell cc-smoke "found it: 7979" --kind progress >/dev/null 2>&1
agent tell cc-smoke "done: listen on 7979" --kind report >/dev/null 2>&1
a=$(op wait --task T-ASK --new --as cc-smoke --timeout 10s 2>&1)
b=$(op wait --task T-ASK --new --as cc-smoke --timeout 10s 2>&1)
c=$(op wait --task T-ASK --new --as cc-smoke --timeout 2s 2>&1); rc=$?
check "wait --new returns the first message" "got: $a" has "$a" "found it: 7979"
check "wait --new returns the next message, not the first again" "got: $b" bash -c '! grep -q "found it" <<<"$1" && grep -q "listen on 7979" <<<"$1"' _ "$b"
check "wait --new with nothing new times out (exit 4)" "exit $rc: $c" test $rc = 4
out=$(op wait --task T-ASK --new --as cc-other --timeout 5s 2>&1)
check "another session's wait --new starts from the beginning" "got: $out" has "$out" "7878 or 7979"
out=$(agent tell cc-smoke "x" --kind gossip 2>&1)
check "tell refuses an unknown kind" "accepted: $out" has "$out" "gossip"

echo "== verified agent identity"
out=$(FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$RELAY FLEET_AGENT=agent-01 FLEET_AGENT_KEY=0000 "$B/fleetctl" memory recall deploys 2>&1)
check "a forged X-Fleet-Agent is refused" "got: $out" has "$out" "does not verify"
out=$(FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$RELAY FLEET_AGENT=agent-02 FLEET_AGENT_KEY=$KEY "$B/fleetctl" status 2>&1)
check "agent-01's key does not prove agent-02" "got: $out" has "$out" "does not verify"
out=$(agent tell cc-smoke "hi" --from agent-02 2>&1)
check "a verified agent does not speak as another" "got: $out" has "$out" "does not speak as agent-02"
out=$(FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$RELAY "$B/fleetctl" memory recall deploys 2>&1)
check "the relay token without a key gets no memory" "got: $out" has "$out" "FLEET_AGENT_KEY"

echo "== memory"
out=$(agent memory recall "when do deploys go out" 2>&1)
check "an agent on the operator's task recalls through the CLI" "got: $out" has "$out" "21:00 from pc2"
check "the gitloom key stays in fleetd" "agent saw the key: $out" bash -c '! grep -q "$1" <<<"$2"' _ $GITLOOM_KEY "$out"
mcp=$(printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory_recall","arguments":{"query":"deploys"}}}' |
  agent mcp 2>&1)
check "fleetctl mcp lists memory_recall and the gateway's tools" "tools/list: $(jq -c 'select(.id==2)' <<<"$mcp" 2>/dev/null)" \
  bash -c 'jq -e "select(.id==2) | [.result.tools[].name] | index(\"memory_recall\") and index(\"memory_suggest\") and index(\"karmax_call\")" <<<"$1" >/dev/null' _ "$mcp"
check "memory recall over fleetctl mcp (stdio JSON-RPC)" "tools/call: $(jq -c 'select(.id==3)' <<<"$mcp" 2>/dev/null)" \
  bash -c 'jq -e "select(.id==3) | (.result.isError | not) and (.result.content[0].text | contains(\"21:00 from pc2\"))" <<<"$1" >/dev/null' _ "$mcp"
out=$(agent memory suggest "The smoke test runs before each deploy." --why "it caught a bug" 2>&1)
check "memory suggest goes to the orchestrator" "suggest said: $out; relay log: $(tail -c 300 "$RELAY_LOG")" grep -q "Memory suggestion from agent-01" "$RELAY_LOG"

echo "== KARMAX gateway"
out=$(agent karmax tools --json 2>&1)
check "the gateway lists a read and a write tool" "got: $out" \
  bash -c 'jq -e "(map(select(.name==\"mail.search\"))[0].class==\"read\") and (map(select(.name==\"mail.send\"))[0].class==\"write\")" <<<"$1" >/dev/null' _ "$out"
out=$(agent karmax call mail.search q=invoices --reason "find the invoice" --json 2>&1)
last=$(tail -n1 "$R/fakes/karmax-calls.jsonl" 2>/dev/null)
check "a gateway read passes through at once" "got: $out" bash -c 'jq -e ".result.ok and .result.output.echo.q==\"invoices\"" <<<"$1" >/dev/null' _ "$out"
check "KARMAX's token is added by fleetd, not the agent" "KARMAX saw: $last" bash -c 'jq -e ".tool==\"mail.search\" and .token_ok" <<<"$1" >/dev/null' _ "$last"
check "the agent never sees KARMAX's token" "in its output: $out" bash -c '! grep -q "$1" <<<"$2"' _ $KARMAX_TOKEN "$out"
agent karmax call mail.send to=ram@example.com --reason "send the invoice" --timeout 1m --json > "$R/write.out" 2> "$R/write.err" & WR=$!
id=
for _ in $(seq 50); do
  id=$(op approvals --json 2>/dev/null | jq -r '.[] | select(.tool=="mail.send" and .state=="pending") | .id' | head -n1)
  [ -n "$id" ] && break; sleep 0.2
done
sent() { grep -c '"tool": "mail.send"' "$R/fakes/karmax-calls.jsonl" 2>/dev/null || true; }
check "a gateway write is filed for approval" "no pending request in fleetctl approvals" test -n "$id"
sleep 1
check "a gateway write does not run before approval" "KARMAX saw $(sent) sends; agent: $(cat "$R/write.out")" bash -c 'kill -0 $1 2>/dev/null && [ "$2" = 0 ]' _ $WR "$(sent)"
out=$(op approve "$id" 2>&1)
wait $WR; rc=$?
check "the write runs once approved and the agent's call returns its result" "exit $rc: $(cat "$R/write.out"); approve: $out" \
  bash -c 'jq -e ".result.ok and .result.output.echo.to==\"ram@example.com\"" "$1" >/dev/null' _ "$R/write.out"
out=$(op approve "$id" 2>&1)
check "an approval runs only once" "KARMAX saw $(sent) sends; second approve: $out" test "$(sent)" = 1
agent karmax call mail.send to=ram@example.com --reason "send it again" --timeout 1m > "$R/deny.out" 2>&1 & DN=$!
id2=
for _ in $(seq 50); do
  id2=$(op approvals --json 2>/dev/null | jq -r '.[] | select(.tool=="mail.send" and .state=="pending") | .id' | head -n1)
  [ -n "$id2" ] && break; sleep 0.2
done
op deny "$id2" --reason "it went already" >/dev/null 2>&1
wait $DN; rc=$?
check "a denied write is refused with the reason and never runs" "exit $rc, KARMAX saw $(sent) sends: $(cat "$R/deny.out")" \
  bash -c '[ $1 != 0 ] && [ "$2" = 1 ] && grep -q "it went already" "$3"' _ $rc "$(sent)" "$R/deny.out"
out=$(agent approve "$id" 2>&1)
check "an agent cannot approve" "got: $out" has "$out" "scope relay"
out=$(agent karmax call system.exec cmd=ls --reason x 2>&1)
check "a code-running tool is always denied" "got: $out" has "$out" "refused"

echo "== scopes: members and the console get none of the operator's data"
out=$(member whoami 2>&1)
check "the tailnet member is a member" "whoami: $out" has "$out" "scope member"
for c in "memory recall deploys" "memory suggest x" "karmax tools" "karmax call mail.search q=x --reason x"; do
  out=$(member $c 2>&1)
  check "a member is refused: $c" "got: $out" bash -c 'grep -q "403\|may:\|not for scope" <<<"$1" && ! grep -q "21:00" <<<"$1"' _ "$out"
  out=$(consoleTok $c 2>&1)
  check "the console token is refused: $c" "got: $out" bash -c 'grep -q "403\|may:\|not for scope" <<<"$1" && ! grep -q "21:00" <<<"$1"' _ "$out"
done

echo "== an agent off the operator's work gets none of it"
op done agent-01 --task T-ASK --keep-session >/dev/null 2>&1
out=$(op model agent-01 --json)
check "the task's model ends with the task" "model: $out" test "$(jq -r '.task // ""' <<<"$out")" = ""
out=$(agent memory recall deploys 2>&1)
check "an agent with no task is refused memory" "got: $out" has "$out" "no task"

# ---------------------------------------------------------------------------
# Arriving on branches. Each part runs once its command is in this build, and
# says it skipped until then: the script grows with the merges.
echo "== on branches (skipped until merged)"
usage=$("$B/fleetctl" help)
if has "$usage" "secret put"; then # v2-secrets-2
  op run "Use the deploy key." --agent agent-01 --task T-SEC >/dev/null 2>&1
  printf 's3cr3t-smoke\n' | op secret put DEPLOY_KEY >/dev/null 2>&1
  op secret grant DEPLOY_KEY --task T-SEC >/dev/null 2>&1
  out=$(agent secret exec K=DEPLOY_KEY -- sh -c 'test "$K" = s3cr3t-smoke && echo matched' 2>&1)
  check "secret exec hands a granted agent the value in its environment" "got: $out" has "$out" matched
  out=$(op secret ls --json 2>&1)
  check "secret ls never shows a value" "got: $out" bash -c 'grep -q DEPLOY_KEY <<<"$1" && ! grep -q s3cr3t-smoke <<<"$1"' _ "$out"
  out=$(member secret ls 2>&1)
  check "a member is refused secrets" "got: $out" bash -c '! grep -q DEPLOY_KEY <<<"$1"' _ "$out"
  op done agent-01 --task T-SEC --keep-session >/dev/null 2>&1
  out=$(agent secret exec K=DEPLOY_KEY -- true 2>&1); rc=$?
  check "a task's grant ends when the task closes" "exit $rc: $out" test $rc != 0
else
  echo "SKIP secrets: fleetctl secret is not in this build (v2-secrets-2)"
fi
if has "$usage" "  cp " || has "$usage" "  sync "; then # v2-files
  echo "SKIP files: fleetctl cp/sync is in this build but has no check here yet: add it"
else
  echo "SKIP files: fleetctl cp/sync is not in this build (v2-files)"
fi
if op status --json 2>/dev/null | grep -q '"slot'; then # v2-slots
  out=$(op status --json)
  check "slots: agent-02 with slots: 2 is two workers, agent-02 and agent-02:2" "status: $(jq -c '[.agents[].agent]' <<<"$out")" \
    bash -c 'jq -e "[.agents[].agent] | index(\"agent-02\") and index(\"agent-02:2\")" <<<"$1" >/dev/null' _ "$out"
  check "slots: agent-01 with slots: 1 is one worker" "status: $(jq -c '[.agents[].agent]' <<<"$out")" \
    bash -c 'jq -e "[.agents[] | select(.slot_of==\"agent-01\")] | length == 1" <<<"$1" >/dev/null' _ "$out"
  check "slots: agent-02:2 says it is slot 2 of agent-02" "status: $(jq -c '.agents[] | select(.agent=="agent-02:2")' <<<"$out")" \
    bash -c 'jq -e ".agents[] | select(.agent==\"agent-02:2\") | .slot_of==\"agent-02\" and .slot==2" <<<"$1" >/dev/null' _ "$out"
  # agent-02's slot 2 as inside its container: the container's key, its own name.
  KEY2=$(python3 -c 'import hmac,hashlib,sys; print(hmac.new(sys.argv[1].encode(), b"agent:agent-02", hashlib.sha256).hexdigest())' $FULL)
  slot() { FLEET_CONFIG=$R/none FLEETD_URL=http://$API FLEET_TOKEN=$RELAY FLEET_AGENT=$1 FLEET_NAME=$1 FLEET_AGENT_KEY=$KEY2 "$B/fleetctl" "${@:2}"; }
  out=$(slot agent-02:2 whoami 2>&1)
  check "slots: a slot proves itself with its container's key" "whoami: $out" bash -c '! grep -qi "verify\|no worker" <<<"$1"' _ "$out"
  out=$(slot agent-02:9 whoami 2>&1)
  check "slots: the key proves no slot the config does not have" "got: $out" has "$out" "no worker"
  op give agent-02:2 --task T-SLOT "Say where you are." >/dev/null 2>&1
  slot agent-02:2 tell cc-smoke "slot two, on it" --kind progress >/dev/null 2>&1
  out=$(op wait --task T-SLOT --new --as cc-smoke --timeout 10s 2>&1)
  check "slots: a slot's message is filed under its own task" "wait said: $out" has "$out" "slot two, on it"
  out=$(op tasks --open --json 2>/dev/null)
  check "slots: the task is the slot's, not its agent's slot 1" "open: $(jq -c '[.[] | {id, agent}]' <<<"$out" 2>/dev/null)" \
    bash -c 'jq -e "map(select(.id==\"T-SLOT\"))[0].agent==\"agent-02:2\"" <<<"$1" >/dev/null' _ "$out"
  op done agent-02:2 --task T-SLOT --keep-session >/dev/null 2>&1
else
  echo "SKIP slots: status has no slots in this build (v2-slots)"
fi
echo "SKIP durability: no check yet (v2-durable): add it when it merges"

# ---------------------------------------------------------------------------
if [ -s "$R/fakes/violations" ]; then
  fail "the fakes saw only the routes fleetd may call" "$(cat "$R/fakes/violations")"
else
  pass "the fakes saw only the routes fleetd may call"
fi
[ -n "$VERBOSE" ] && { echo "== fleetd log"; cat "$R/fleetd.log"; }
echo "== $PASSES passed, $FAILS failed in ${SECONDS}s"
[ $FAILS = 0 ]
