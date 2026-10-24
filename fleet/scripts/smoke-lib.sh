# Shared by smoke.sh and smoke-v2.sh: source it, then call smoke_fakes.

# smoke_fakes writes the fakes into $1/bin: a `docker` that runs `exec`
# locally (its -e variables set) and answers inspect and stats, and a
# `claude` that lists sessions from $HOME/.claude/sessions and, as `claude
# -p` (a relay), logs its prompt to $RELAY_LOG and says the SendMessage
# went through.
smoke_fakes() {
local R=$1
# Run inside an agent's own tmux, tmux would act on that server: the
# smoke's new-session and its kill-server at the end must not.
unset TMUX TMUX_PANE
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
