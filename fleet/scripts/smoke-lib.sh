# Shared by smoke.sh and smoke-v2.sh: source it, then call smoke_fakes.

# smoke_fakes writes the fakes into $1/bin: a `docker` that runs `exec`
# locally (its -e variables set) and answers inspect and stats, and a
# `claude` that lists sessions from $HOME/.claude/sessions and, as `claude
# -p` (a relay), logs its prompt to $RELAY_LOG and says the SendMessage
# went through.
smoke_fakes() {
local R=$1
# Nothing of the machine it runs on: inside an agent's container, $TMUX is
# the agent's own tmux (the smoke's kill-server would end its session), and
# FLEETD_URL and FLEET_TOKEN are the live fleetd's, which fleetctl would
# talk to instead of the smoke's.
unset TMUX TMUX_PANE FLEETD_URL FLEET_TOKEN FLEET_AGENT FLEET_NAME FLEET_SESSION FLEET_AGENT_KEY CLAUDE_CODE_SESSION_ID
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
if [ "$1" = -p ]; then
  echo "role=$FLEET_ROLE args=$*" >> "$RELAY_LOG"; p=$(cat); printf '%s\n' "$p" >> "$RELAY_LOG"
  # A relay is delivered when its SendMessage says so (stream-json).
  to=$(printf '%s' "$p" | sed -n 's/.*session named "\([^"]*\)".*/\1/p' | head -n1)
  printf '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"SendMessage","input":{"to":"%s"}}]}}\n' "$to"
  printf '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"{\\"success\\":true}"}]}}\n'
  exit 0
fi
EOF
chmod +x "$R/bin/"*
}

# free_port prints a TCP port nobody listens on now: agents share a host's
# network, and two smoke runs on fixed ports would talk to each other's fleetd.
free_port() {
python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}
