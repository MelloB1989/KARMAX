#!/usr/bin/env bash
# Start the whole demo stack locally. Safe to run from anywhere.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p demo/run demo/logs demo/data
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin

wait_port() { # name host:port
  for _ in $(seq 1 60); do
    (exec 3<>"/dev/tcp/${2%:*}/${2#*:}") 2>/dev/null && { echo "  ✓ $1 up on $2"; return 0; }
    sleep 0.5
  done
  echo "  ✗ $1 did not open $2 — see demo/logs/" >&2; return 1
}
start() { # name cmd...
  local name=$1; shift
  nohup "$@" >"demo/logs/$name.log" 2>&1 &
  echo $! >"demo/run/$name.pid"
}

[ -x bin/fakegithub ] || { echo "building fakegithub"; go build -o bin/fakegithub ./cmd/fakegithub; }
[ -x bin/karmax ]     || { echo "building karmax";     CGO_ENABLED=1 go build -o bin/karmax ./cmd/karmax; }
[ -x bin/payments ]   || { echo "building payments";   go build -o bin/payments ./demo/payments; }
chmod +x demo/*.sh

start fakegithub bin/fakegithub -addr 127.0.0.1:8787 -webhook http://127.0.0.1:9090/orgops/github -secret dev -seed acme
wait_port "fake GitHub" 127.0.0.1:8787

if [ -f demo/chat/server.py ]; then
  start chat python3 demo/chat/server.py
  wait_port "chat" 127.0.0.1:9393
else
  echo "  ! demo/chat/server.py not present yet (task B) — skipping chat; scripts fall back to KARMAX logs" >&2
fi

start payments bin/payments -addr 127.0.0.1:9494
wait_port "payments" 127.0.0.1:9494

# Model auth for the harness (token lives outside the repo; never printed).
[ -f ~/.config/karmax-demo/env ] && { set -a; . ~/.config/karmax-demo/env; set +a; }
start karmax bin/karmax start -c demo/karmax.yaml
wait_port "KARMAX webhooks" 127.0.0.1:9090

cat <<EOT

Stack is up.
  chat page        http://127.0.0.1:9393
  fake GitHub      http://127.0.0.1:8787/_fake/state
  KARMAX webhooks  http://127.0.0.1:9090
  payments         http://127.0.0.1:9494/health
  logs             demo/logs/*.log
Next: demo/scene1.sh, demo/scene2.sh   (reset: demo/down.sh)
EOT
