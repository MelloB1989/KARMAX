#!/bin/bash
# End-to-end smoke test of fleetd and fleetctl without Docker or Claude:
# real binaries, real tmux, a fake `docker` that runs `exec` locally and a
# fake `claude`. Assign, done (archive + verify + rotate), relay, token
# scopes, OTLP, usage, quota. Run from fleet/: scripts/smoke.sh
set -u
cd "$(dirname "$0")/.."
R=$(mktemp -d); B=$R/fleetbin
go build -o "$B/" ./cmd/... || exit 1
mkdir -p "$R"/{bin,home/.claude/sessions,home/.claude/fleet,home/.claude/projects/-work,work,state}
chmod 700 "$R/state"
cat > "$R/bin/docker" <<'EOF'
#!/bin/bash
case "$1" in
  inspect) echo true ;;
  stats) echo '{"Name":"agent-01","CPUPerc":"1.00%","MemUsage":"100MiB / 15GiB","MemPerc":"0.6%"}' ;;
  exec) shift; env=(); while [[ "$1" == -* ]]; do if [[ "$1" == -e ]]; then env+=("$2"); shift; fi; shift; done; shift; exec env "${env[@]}" "$@" ;;
esac
EOF
cat > "$R/bin/claude" <<'EOF'
#!/bin/bash
if [ "$1" = agents ]; then
  printf '['; sep=; for f in "$HOME"/.claude/sessions/*.json; do [ -e "$f" ] || continue; printf '%s' "$sep"; cat "$f"; sep=,; done; printf ']\n'; exit 0
fi
if [ "$1" = -p ]; then echo "role=$FLEET_ROLE args=$*" >> "$RELAY_LOG"; cat >> "$RELAY_LOG"; exit 0; fi
EOF
chmod +x "$R/bin/"*
export TMUX_TMPDIR=$(mktemp -d "${XDG_RUNTIME_DIR:-/tmp}/fl.XXXX")  # sockets need a short path
export HOME=$R/home FLEET_WORK=$R/work PATH=$R/bin:$PATH RELAY_LOG=$R/relay.log
cat > "$R/state/fleet.yaml" <<EOF
home: $R/home
env_file: $R/env
state_dir: $R/state
listen: 127.0.0.1:17878
otlp_listen: 127.0.0.1:17318
hosts: {kali: {exec: [$R/bin/docker]}}
orchestrator: {host: kali, token_env: CLAUDE_TOKEN_KARMAX}
agents:
  agent-01: {host: kali, token_env: CLAUDE_TOKEN_AGENT_01}
karmax: {notify: false, api_url: "http://127.0.0.1:1"}
thresholds: {tick: 2s}
EOF
printf 'FLEET_TOKEN=full\nFLEET_RELAY_TOKEN=relay\nCLAUDE_TOKEN_KARMAX=k\nCLAUDE_TOKEN_AGENT_01=a1\n' > "$R/env"; chmod 600 "$R/env"
export FLEET_CONFIG=$R/state/fleet.yaml

tmux new-session -d -s main -e FLEET_AGENT=agent-01 "exec sleep 600"; sleep 0.3
pid=$(tmux list-panes -t main -F '#{pane_pid}')
echo "{\"pid\":$pid,\"kind\":\"interactive\",\"sessionId\":\"s1\",\"name\":\"agent-01\",\"status\":\"idle\"}" > "$HOME/.claude/sessions/$pid.json"
mkdir -p "$R/work/.fleet"; echo s1 > "$R/work/.fleet/current"
cat > "$HOME/.claude/projects/-work/s1.jsonl" <<'EOF'
{"type":"user","sessionId":"s1","cwd":"/work","timestamp":"2026-10-08T10:00:00Z","turnOrigin":"peer","message":{"role":"user","content":"fix it"}}
{"type":"assistant","sessionId":"s1","timestamp":"2026-10-08T10:10:00Z","message":{"id":"m1","model":"claude-sonnet","content":[{"type":"text","text":"fixed, pushed fleet/agent-01/fix"}],"usage":{"output_tokens":77}}}
EOF
echo '{"t":1791427770,"event":"SessionStart","session":"s1"}' > "$HOME/.claude/fleet/events.jsonl"
echo '{"session_id":"s1","context_window":{"used_percentage":12},"rate_limits":{"five_hour":{"used_percentage":40,"resets_at":1891430000}}}' > "$HOME/.claude/fleet/status.json"

"$B/fleetd" > "$R/fleetd.log" 2>&1 & FPID=$!
trap 'kill $FPID 2>/dev/null; tmux kill-server 2>/dev/null; rm -rf "$R" "$TMUX_TMPDIR"' EXIT
sleep 3
echo "== ls"; "$B/fleetctl" ls
echo "== assign + done"; "$B/fleetctl" assign agent-01 --task T42 && "$B/fleetctl" done agent-01 --task T42 --branch fleet/agent-01/fix
sleep 5
echo "== next file: [$(cat "$R/work/.fleet/next" 2>&1)] pane alive: $(tmux has-session -t main 2>/dev/null && echo yes || echo no)"
echo "== transcript left in container: $(ls "$HOME/.claude/projects/-work/")"
echo "== archive ls"; "$B/fleetctl" archive ls
echo "== history"; "$B/fleetctl" history agent-01
echo "== tell"; FLEET_TOKEN=relay FLEETD_URL=http://127.0.0.1:17878 "$B/fleetctl" tell agent-01 "please rebase" --from karmax; head -3 "$R/relay.log"
echo "== an unknown sender is refused:"; FLEET_TOKEN=relay FLEETD_URL=http://127.0.0.1:17878 "$B/fleetctl" tell agent-01 "x" --from mallory
echo "== relay token cannot assign:"; FLEET_TOKEN=relay FLEETD_URL=http://127.0.0.1:17878 "$B/fleetctl" assign agent-01 --task X
echo "== otlp"; curl -s -o /dev/null -w '%{http_code}\n' -H 'Authorization: Bearer relay' -H 'Content-Type: application/json' \
  -d '{"resourceLogs":[{"resource":{"attributes":[{"key":"fleet.agent","value":{"stringValue":"agent-01"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"'$(date +%s)'000000000","attributes":[{"key":"event.name","value":{"stringValue":"api_request"}},{"key":"model","value":{"stringValue":"claude-sonnet"}},{"key":"output_tokens","value":{"intValue":"500"}},{"key":"cost_usd","value":{"doubleValue":0.02}}]}]}]}]}' \
  http://127.0.0.1:17318/v1/logs
echo "== usage"; "$B/fleetctl" usage
echo "== events"; "$B/fleetctl" events agent-01 --since 1000d | head -20
echo "== quota"; "$B/fleetctl" quota
echo "== fleetd log"; cat "$R/fleetd.log"
